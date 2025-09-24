/*
Copyright 2024 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

var (
	ctx             context.Context
	cancel          context.CancelFunc
	grpcClient      extProcPb.ExternalProcessorClient
	grpcConn        *grpc.ClientConn
	backendServer   *httptest.Server
	processorServer *Server
	grpcServer      *grpc.Server
	listener        *bufconn.Listener
)

func getBlockChannel(cb func(), duration time.Duration) chan bool {
	block := make(chan bool)
	go func() {
		cb()
		block <- false
	}()
	return block
}

func shouldBlock(cb func(), duration time.Duration, msg string) {
	block := getBlockChannel(cb, duration)
	Consistently(block, duration).ShouldNot(Receive(BeFalse()), msg)
}

func shouldNotBlock(cb func(), duration time.Duration, msg string) {
	block := getBlockChannel(cb, duration)
	Eventually(block, duration).Should(Receive(BeFalse()), msg)
}

func TestForwarderIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Request Forwarder Integration Suite")
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.TODO())

	// --- 1. Start the dummy backend server ---
	backendServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer GinkgoRecover()

		// Simulate different backend behaviors based on path
		switch r.URL.Path {
		case "/api/upload":
			// Handle file upload - verify multipart content
			Expect(r.Header.Get("Content-Type")).To(ContainSubstring("multipart/form-data"))

			// Read and echo back the upload
			body, err := io.ReadAll(r.Body)
			Expect(err).NotTo(HaveOccurred())

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Backend-Processed", "upload")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(fmt.Sprintf(`{"upload_size": %d, "status": "received"}`, len(body)))) // nolint: errcheck

		case "/api/download":
			// Handle large file download - trigger streaming
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", "attachment; filename=test-file.bin")
			w.Header().Set("X-Backend-Processed", "download")

			// Write a large response in chunks to simulate streaming
			largeContent := strings.Repeat("DOWNLOAD_CHUNK_DATA_", 1000) // ~20KB
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(largeContent)))
			w.WriteHeader(http.StatusOK)

			// Write in chunks to simulate real streaming
			data := []byte(largeContent)
			chunkSize := 1024
			for i := 0; i < len(data); i += chunkSize {
				end := i + chunkSize
				if end > len(data) {
					end = len(data)
				}
				w.Write(data[i:end]) // nolint: errcheck
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(1 * time.Millisecond) // Simulate network delay
			}

		case "/api/chat":
			// Handle normal JSON request
			Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))

			body, err := io.ReadAll(r.Body)
			Expect(err).NotTo(HaveOccurred())

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Backend-Processed", "chat")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(fmt.Sprintf(`{"response": "processed", "echo": %s}`, body))) // nolint: errcheck

		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("Not Found")) // nolint: errcheck
		}
	}))

	// --- 2. Create forwarder configuration ---
	forwardingConfig := map[string]string{
		"/api/": backendServer.URL,
	}

	// --- 3. Start the gRPC server (gateway forwarder) in-memory ---
	buffer := 1024 * 1024
	listener = bufconn.Listen(buffer)
	grpcServer = grpc.NewServer()

	// Instantiate the gateway forwarder server
	processorServer = &Server{
		httpClient:       &http.Client{Timeout: 30 * time.Second},
		forwardingConfig: forwardingConfig,
		// activeForwards is initialized as zero value (empty SyncMap)
	}
	extProcPb.RegisterExternalProcessorServer(grpcServer, processorServer)

	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			log.Printf("gRPC server exited with error: %v", err)
		}
	}()

	// --- 4. Create the gRPC client (simulates Envoy) ---
	var err error
	grpcConn, err = grpc.NewClient("localhost", // Provide a dummy target that pass dns check
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	Expect(err).NotTo(HaveOccurred())
	grpcClient = extProcPb.NewExternalProcessorClient(grpcConn)
})

var _ = AfterSuite(func() {
	if grpcConn != nil {
		grpcConn.Close() // nolint: errcheck
	}
	if grpcServer != nil {
		grpcServer.Stop()
	}
	if backendServer != nil {
		backendServer.Close()
	}
	cancel()
})

var _ = Describe("Gateway Request Forwarder", func() {
	var stream extProcPb.ExternalProcessor_ProcessClient

	BeforeEach(func() {
		var err error
		stream, err = grpcClient.Process(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if stream != nil {
			stream.CloseSend() // nolint: errcheck
		}
	})

	Context("Normal JSON Request", func() {
		It("should forward a normal JSON request and receive immediate response", func() {
			// 1. Send request headers
			reqHeaders := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{
							Headers: []*configPb.HeaderValue{
								{Key: ":method", RawValue: []byte("POST")},
								{Key: ":path", RawValue: []byte("/api/chat")},
								{Key: ":authority", RawValue: []byte("test.example.com")},
								{Key: "content-type", RawValue: []byte("application/json")},
							},
						},
					},
				},
			}
			Expect(stream.Send(reqHeaders)).To(Succeed())

			// Expect CONTINUE response for headers
			resp, err := stream.Recv()
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetRequestHeaders()).NotTo(BeNil())
			Expect(resp.GetRequestHeaders().GetResponse().GetStatus()).To(Equal(extProcPb.CommonResponse_CONTINUE))

			// 2. Send request body
			requestBody := `{"message": "Hello, world!", "model": "test-model"}`
			reqBody := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestBody{
					RequestBody: &extProcPb.HttpBody{
						Body:        []byte(requestBody),
						EndOfStream: true,
					},
				},
			}
			Expect(stream.Send(reqBody)).To(Succeed())

			// 3. Expect immediate response (non-streaming)
			resp, err = stream.Recv()
			Expect(err).NotTo(HaveOccurred())
			immediateResp := resp.GetImmediateResponse()
			Expect(immediateResp).NotTo(BeNil())
			Expect(immediateResp.GetStatus().GetCode().String()).To(Equal("OK"))

			// Verify backend processing
			Expect(immediateResp.GetBody()).To(ContainSubstring("processed"))
			Expect(immediateResp.GetBody()).To(ContainSubstring("Hello, world!"))

			// Check headers
			headers := immediateResp.GetHeaders().GetSetHeaders()
			Expect(getHeaderValue(headers, "Content-Type")).To(Equal("application/json"))
			Expect(getHeaderValue(headers, "X-Backend-Processed")).To(Equal("chat"))
			Expect(getHeaderValue(headers, HeaderStatus)).To(Equal("200"))
		})
	})

	Context("Upload Streaming Request", func() {
		It("should handle multipart file upload with streaming", func() {
			// 1. Send request headers for file upload
			reqHeaders := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{
							Headers: []*configPb.HeaderValue{
								{Key: ":method", RawValue: []byte("POST")},
								{Key: ":path", RawValue: []byte("/api/upload")},
								{Key: ":authority", RawValue: []byte("test.example.com")},
								{Key: "content-type", RawValue: []byte("multipart/form-data; boundary=test-boundary")},
							},
						},
					},
				},
			}
			Expect(stream.Send(reqHeaders)).To(Succeed())

			// Expect CONTINUE response for headers
			shouldNotBlock(func() {
				defer GinkgoRecover()

				resp, err := stream.Recv()
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.GetRequestHeaders()).NotTo(BeNil())
				Expect(resp.GetRequestHeaders().GetResponse().GetStatus()).To(Equal(extProcPb.CommonResponse_CONTINUE))
			}, 1*time.Second, "Should not timeout on waiting CONTINUE response.")

			// 2. Create multipart upload data
			var uploadBuffer bytes.Buffer
			writer := multipart.NewWriter(&uploadBuffer)
			writer.SetBoundary("test-boundary") // nolint: errcheck

			part, err := writer.CreateFormFile("file", "test.txt")
			Expect(err).NotTo(HaveOccurred())

			testFileContent := strings.Repeat("FILE_CONTENT_CHUNK_", 100) // ~2KB
			_, err = part.Write([]byte(testFileContent))
			Expect(err).NotTo(HaveOccurred())
			writer.Close() // nolint: errcheck

			uploadData := uploadBuffer.Bytes()
			chunkSize := 512

			// 3. Send upload data in chunks
			for i := 0; i < len(uploadData); i += chunkSize {
				end := i + chunkSize
				isLast := end >= len(uploadData)
				if isLast {
					end = len(uploadData)
				}

				reqBody := &extProcPb.ProcessingRequest{
					Request: &extProcPb.ProcessingRequest_RequestBody{
						RequestBody: &extProcPb.HttpBody{
							Body:        uploadData[i:end],
							EndOfStream: isLast,
						},
					},
				}
				Expect(stream.Send(reqBody)).To(Succeed())

				if isLast {
					break
				}

				// For intermediate chunks, expect CONTINUE
				shouldNotBlock(func() {
					defer GinkgoRecover()

					resp, err := stream.Recv()
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.GetRequestBody()).NotTo(BeNil())
					Expect(resp.GetRequestBody().GetResponse()).NotTo(BeNil())
				}, 1*time.Second, fmt.Sprintf("Should not timeout on waiting intermediate chunk (%d of %d).", i+1, int(math.Ceil(float64(len(uploadData))/float64(chunkSize)))))
			}

			// 4. For the final chunk, expect immediate response
			shouldNotBlock(func() {
				defer GinkgoRecover()

				resp, err := stream.Recv()
				Expect(err).NotTo(HaveOccurred())
				immediateResp := resp.GetImmediateResponse()
				Expect(immediateResp).NotTo(BeNil(), fmt.Sprintf("Unexpected response: %v", resp))
				Expect(immediateResp.GetStatus().GetCode().String()).To(Equal("OK"))

				// Verify upload was processed
				Expect(immediateResp.GetBody()).To(ContainSubstring("upload_size"))
				Expect(immediateResp.GetBody()).To(ContainSubstring("received"))

				// Check headers
				headers := immediateResp.GetHeaders().GetSetHeaders()
				Expect(getHeaderValue(headers, "X-Backend-Processed")).To(Equal("upload"))
				Expect(getHeaderValue(headers, HeaderStatus)).To(Equal("200"))
			}, 1*time.Second, "Should not timeout on waiting final chunk.")
		})
	})

	Context("Download Streaming Request", func() {
		It("should handle large file download with streaming response", func() {
			// 1. Send request headers for file download
			reqHeaders := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{
							Headers: []*configPb.HeaderValue{
								{Key: ":method", RawValue: []byte("GET")},
								{Key: ":path", RawValue: []byte("/api/download")},
								{Key: ":authority", RawValue: []byte("test.example.com")},
								{Key: "accept", RawValue: []byte("*/*")},
							},
						},
					},
				},
			}
			Expect(stream.Send(reqHeaders)).To(Succeed())

			// Expect CONTINUE response for headers
			resp, err := stream.Recv()
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetRequestHeaders()).NotTo(BeNil())
			Expect(resp.GetRequestHeaders().GetResponse().GetStatus()).To(Equal(extProcPb.CommonResponse_CONTINUE))

			// 2. Send empty body (GET request)
			reqBody := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestBody{
					RequestBody: &extProcPb.HttpBody{
						EndOfStream: true,
					},
				},
			}
			Expect(stream.Send(reqBody)).To(Succeed())

			// 3. Expect CONTINUE_AND_REPLACE response (streaming initiation)
			resp, err = stream.Recv()
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetRequestHeaders()).NotTo(BeNil())
			Expect(resp.GetRequestHeaders().GetResponse().GetStatus()).To(Equal(extProcPb.CommonResponse_CONTINUE_AND_REPLACE))

			// 4. Receive response headers
			resp, err = stream.Recv()
			Expect(err).NotTo(HaveOccurred())
			headersResp := resp.GetResponseHeaders()
			Expect(headersResp).NotTo(BeNil())

			// Verify response headers
			headerMutation := headersResp.GetResponse().GetHeaderMutation()
			Expect(getHeaderValue(headerMutation.GetSetHeaders(), HeaderStatus)).To(Equal("200"))
			Expect(getHeaderValue(headerMutation.GetSetHeaders(), "Content-Type")).To(Equal("application/octet-stream"))
			Expect(getHeaderValue(headerMutation.GetSetHeaders(), "Content-Disposition")).To(ContainSubstring("attachment"))
			Expect(getHeaderValue(headerMutation.GetSetHeaders(), "X-Backend-Processed")).To(Equal("download"))

			// 5. Collect streaming response body chunks
			var receivedData []byte
			var trailerReceived bool

			for !trailerReceived {
				resp, err = stream.Recv()
				Expect(err).NotTo(HaveOccurred())

				switch r := resp.Response.(type) {
				case *extProcPb.ProcessingResponse_ResponseBody:
					bodyData := r.ResponseBody.GetResponse().GetBodyMutation().GetBody()
					receivedData = append(receivedData, bodyData...)

				case *extProcPb.ProcessingResponse_ResponseTrailers:
					trailerReceived = true
					Expect(r.ResponseTrailers).NotTo(BeNil())

				default:
					Fail(fmt.Sprintf("Unexpected response type: %T", r))
				}
			}

			// 6. Verify the complete downloaded content
			expectedContent := strings.Repeat("DOWNLOAD_CHUNK_DATA_", 1000)
			Expect(len(receivedData)).To(Equal(len(expectedContent)))
			Expect(string(receivedData)).To(Equal(expectedContent))
		})
	})

	Context("Error Handling", func() {
		It("should handle backend server errors gracefully", func() {
			// Test with a path that returns 404
			reqHeaders := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{
							Headers: []*configPb.HeaderValue{
								{Key: ":method", RawValue: []byte("GET")},
								{Key: ":path", RawValue: []byte("/api/nonexistent")},
								{Key: ":authority", RawValue: []byte("test.example.com")},
							},
						},
					},
				},
			}
			Expect(stream.Send(reqHeaders)).To(Succeed())

			// Expect CONTINUE response for headers
			_, err := stream.Recv()
			Expect(err).NotTo(HaveOccurred())

			// Send empty body
			reqBody := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestBody{
					RequestBody: &extProcPb.HttpBody{
						EndOfStream: true,
					},
				},
			}
			Expect(stream.Send(reqBody)).To(Succeed())

			// Expect immediate response with 404 status
			resp, err := stream.Recv()
			Expect(err).NotTo(HaveOccurred())
			immediateResp := resp.GetImmediateResponse()
			Expect(immediateResp).NotTo(BeNil())
			Expect(immediateResp.GetStatus().GetCode().String()).To(Equal("NotFound"))
		})
	})
})

// Helper function to extract header value from header options
func getHeaderValue(headers []*configPb.HeaderValueOption, key string) string {
	for _, headerOpt := range headers {
		if header := headerOpt.GetHeader(); header != nil && header.GetKey() == key {
			if header.GetValue() != "" {
				return header.GetValue()
			}
			return string(header.GetRawValue())
		}
	}
	return ""
}
