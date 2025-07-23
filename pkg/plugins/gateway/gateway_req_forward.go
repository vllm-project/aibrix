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
	"io"
	"net/http"
	"strconv"
	"strings"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"k8s.io/klog/v2"
)

const (
	forwardPreparing forwardingPhase = iota
	forwarding
	forwardContinuing
	forwarded
	forwardRespondInitiated
	forwardRespoondContinuing
	forwardResponded
)

type forwardingPhase int

func (phase forwardingPhase) String() string {
	return []string{
		"forwardPreparing",
		"forwarding",
		"forwardContinuing",
		"forwarded",
		"forwardRespondInitiated",
		"forwardRespoondContinuing",
		"forwardResponded",
	}[phase]
}

// streamingRequest tracks metadata for active streaming uploads
type forwardingRequest struct {
	requestID    string
	targetURL    string
	httpMethod   string
	contentType  string
	responseChan chan *extProcPb.ProcessingResponse
	writer       *io.PipeWriter // For request streaming
	phase        forwardingPhase
}

func (req *forwardingRequest) DoneForward(err error) {
	writer := req.writer
	if writer == nil {
		return
	}
	var closeErr error
	if err != nil {
		closeErr = writer.CloseWithError(err)
	} else {
		closeErr = writer.Close()
	}
	req.writer = nil
	if closeErr != nil {
		klog.ErrorS(closeErr, "failed to close forward stream", "requestID", req.requestID)
	}
}

// forwardRequest prepare the request (Phase 0) and initiate the request forwarding (Phase 1)
func (s *Server) forwardRequest(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, forward *forwardingRequest, streaming bool) *extProcPb.ProcessingResponse {
	klog.V(4).InfoS("Phase 0: prepareForward", "requestID", requestID, "contentType", forward.contentType)

	// Create response channel for async communication
	forward.responseChan = make(chan *extProcPb.ProcessingResponse, 1)
	var reader io.Reader

	if streaming {
		// Create pipe for streaming
		reader, forward.writer = io.Pipe()
	} else if forward.httpMethod == "POST" || forward.httpMethod == "PUT" {
		reader = bytes.NewReader(req.GetRequestBody().GetBody())
	}

	// Create HTTP request with streaming reader
	httpReq, err := http.NewRequestWithContext(ctx, forward.httpMethod, forward.targetURL, reader)
	if err != nil {
		klog.ErrorS(err, "failed to create HTTP request for forwarding", "requestID", requestID)
		forward.DoneForward(err)
		return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorExtendAPI, RawValue: []byte("true")}}},
			"failed to create streaming request to metadata service")
	}

	// Copy headers
	for _, header := range req.GetRequestHeaders().GetHeaders().Headers {
		httpReq.Header.Add(header.Key, header.Value)
	}

	// Start the forward in background
	go s.initiateForward(ctx, requestID, req, forward, httpReq)

	// Process first chunk
	if streaming {
		// ProcessingResponse_ResponseBody for continuing request stream.
		return s.continueForward(ctx, requestID, req, forward)
	}

	// Could be:
	// 1. ProcessingResponse_ResponseHeaders for initated response stream
	// 2. ProcessingResponse_ImmediateResponse for error and short response.
	return <-forward.responseChan
}

// initiateForward initiates the forward (Phase1)
func (s *Server) initiateForward(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, forward *forwardingRequest, forwardReq *http.Request) {
	klog.V(4).InfoS("starting forwarding request", "requestID", requestID, "httpMethod", forward.httpMethod, "targetURL", forward.targetURL)

	// Make the HTTP request
	forward.phase = forwarding
	resp, err := s.httpClient.Do(forwardReq)
	forward.phase = forwarded
	if err != nil {
		klog.ErrorS(err, "failed to forward request to extend api server", "requestID", requestID, "targetURL", forward.targetURL)
		forward.responseChan <- generateErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorExtendAPI, RawValue: []byte("true")}}},
			"extend api server unavailable")
		return
	}

	// Check if response should be streamed based on StatusCode, Content-Type, and Content-Length.
	shouldStreamResponse := s.shouldStreamResponse(resp)
	if shouldStreamResponse {
		// Return CommonResponse_CONTINUE_AND_REPLACE to Envoy to continue the request.
		s.initiateForwardResponse(ctx, requestID, resp, forward)
		return
	}

	// Setup clean up function
	defer func() {
		if err = resp.Body.Close(); err != nil {
			klog.ErrorS(err, "failed to close response body", "requestID", requestID)
		}
	}()

	// Stream response body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		klog.ErrorS(err, "failed to read response from extend api server", "requestID", requestID, "phase", forward.phase)
		forward.responseChan <- generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorExtendAPI, RawValue: []byte("true")}}},
			"failed to read response from extend api server")
		return
	}
	body := string(data)

	// Copy response headers from metadata service
	headers := s.copyResponseHeadersAndStatus(resp)
	forward.responseChan <- &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Status: &envoyTypePb.HttpStatus{
					Code: envoyTypePb.StatusCode(resp.StatusCode),
				},
				Headers: &extProcPb.HeaderMutation{
					SetHeaders: headers,
				},
				Body: body,
			},
		},
	}
	forward.phase = forwardResponded
	klog.V(4).InfoS("forward response completed", "requestID", requestID, "statusCode", resp.StatusCode, "bodyLength", len(body), "phase", forward.phase)
}

// continueForward handles continued streaming upload chunks (Phase 2)
func (s *Server) continueForward(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, forward *forwardingRequest) *extProcPb.ProcessingResponse {
	klog.V(4).InfoS("Phase 2: Continue Forward", "requestID", requestID, "phase", forward.phase)

	body := req.Request.(*extProcPb.ProcessingRequest_RequestBody)
	requestBody := body.RequestBody.GetBody()

	// Write chunk to pipe
	if _, err := forward.writer.Write(requestBody); err != nil {
		klog.ErrorS(err, "failed to write chunk to forward request stream", "requestID", requestID, "phase", forward.phase)
		forward.DoneForward(err)
		return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-error-streaming", RawValue: []byte("true")}}},
			"failed to write chunk to forward request stream")
	}

	// Update phase
	forward.phase = forwardContinuing

	// If this is the last chunk, finalize upload
	if body.RequestBody.EndOfStream {
		forward.phase = forwarded
		forward.DoneForward(nil)
	}

	// Return intermediate response to continue receiving chunks
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{},
			},
		},
	}
}

// initiateForwardResponse initiate a response streamPhase (Phase 4)
func (s *Server) initiateForwardResponse(ctx context.Context, requestID string, resp *http.Response, forward *forwardingRequest) {
	klog.InfoS("Phase 4: Initiate Response", "requestID", requestID, "contentType", resp.Header.Get("Content-Type"), "phase", forward.phase)

	// Use CommonResponse_CONTINUE_AND_REPLACE to override immediate response
	forward.responseChan <- &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					Status: extProcPb.CommonResponse_CONTINUE_AND_REPLACE,
				},
			},
		},
	}

	forward.phase = forwardRespondInitiated

	// Start background goroutine to stream response
	go s.streamForwardResponse(ctx, requestID, resp, forward)
}

// streamForwardResponse streams HTTP response data to a pipe writer (Phase 5)
func (s *Server) streamForwardResponse(ctx context.Context, requestID string, resp *http.Response, forward *forwardingRequest) {
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.ErrorS(err, "failed to close response body", "requestID", requestID)
		}
		close(forward.responseChan) // Close the channel to signal completion
	}()

	klog.InfoS("starting streaming forward response", "requestID", requestID, "phase", forward.phase)

	// Send response headers with CONTINUE_AND_REPLACE to intercept upstream
	headers := s.copyResponseHeadersAndStatus(resp)
	forward.responseChan <- &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
				},
			},
		},
	}

	buffer := make([]byte, 4096) // 4KB chunks
	for {
		select {
		case <-ctx.Done():
			klog.InfoS("forward response streaming cancelled", "requestID", requestID, "phase", forward.phase)
			forward.responseChan <- generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorExtendAPI, RawValue: []byte("true")}}},
				"forward response streaming cancelled")
			return
		default:
		}

		n, err := resp.Body.Read(buffer)
		if n > 0 {
			// Create a copy of the buffer to avoid data corruption when buffer is reused
			bodyData := make([]byte, n)
			copy(bodyData, buffer[:n])

			bodyChunk := &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseBody{
					ResponseBody: &extProcPb.BodyResponse{
						Response: &extProcPb.CommonResponse{
							BodyMutation: &extProcPb.BodyMutation{
								Mutation: &extProcPb.BodyMutation_Body{
									Body: bodyData,
								},
							},
						},
					},
				},
			}
			forward.responseChan <- bodyChunk
		}

		// When the reader is done, send the final empty chunk with the EOS flag.
		if err == io.EOF {
			finalChunk := &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseTrailers{
					ResponseTrailers: &extProcPb.TrailersResponse{},
				},
			}
			forward.responseChan <- finalChunk
			forward.phase = forwardResponded
			klog.InfoS("forward response streaming completed", "requestID", requestID, "phase", forward.phase)
			return
		} else if err != nil {
			klog.ErrorS(err, "error reading HTTP response body", "requestID", requestID, "phase", forward.phase)
			forward.responseChan <- generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorExtendAPI, RawValue: []byte("true")}}},
				"failed to stream forward response")
			return
		}
		forward.phase = forwardContinuing
	}
}

// Helper methods for response streaming and utilities

// shouldStreamResponse determines if a response should be streamed based on content type and size
func (s *Server) shouldStreamResponse(resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}

	contentType := resp.Header.Get("Content-Type")
	contentLength := resp.ContentLength

	// Stream if it's a file download or large response
	return strings.Contains(contentType, "application/octet-stream") ||
		strings.Contains(contentType, "application/pdf") ||
		strings.Contains(contentType, "image/") ||
		strings.Contains(contentType, "video/") ||
		strings.Contains(contentType, "audio/") ||
		contentLength > 1024*1024 // > 1MB
}

// copyResponseHeadersAndStatus copies HTTP response headers to Envoy format
func (s *Server) copyResponseHeadersAndStatus(resp *http.Response) []*configPb.HeaderValueOption {
	headers := make([]*configPb.HeaderValueOption, 0, len(resp.Header)+1)

	// Manually add the status code first
	statusHeader := &configPb.HeaderValueOption{
		Header: &configPb.HeaderValue{
			Key:   HeaderStatus,
			Value: strconv.Itoa(resp.StatusCode), // Convert integer 200 to string "200"
		},
	}
	headers = append(headers, statusHeader)

	// Copy other headers
	for key, values := range resp.Header {
		for _, value := range values {
			hvo := &configPb.HeaderValueOption{
				Header: &configPb.HeaderValue{
					Key:   key,
					Value: value,
				},
			}
			headers = append(headers, hvo)
		}
	}
	return headers
}
