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
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func TestForwardRequest_NonStreaming(t *testing.T) {
	// Create test server that responds with JSON
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result": "success"}`))
	}))
	defer testServer.Close()

	// Create server instance
	server := &Server{
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	// Create forward request
	forward := &forwardingRequest{
		requestID:   "test-request-1",
		targetURL:   testServer.URL,
		httpMethod:  "POST",
		contentType: "application/json",
		phase:       forwardPreparing,
	}

	// Create Envoy request
	requestBody := `{"test": "data"}`
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body: []byte(requestBody),
			},
		},
	}

	// Create request with headers for the forward method to use
	headers := []*configPb.HeaderValue{
		{Key: "Content-Type", Value: "application/json"},
		{Key: "User-Agent", Value: "test-agent"},
	}
	reqHeaders := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: headers,
				},
			},
		},
	}

	// Manually set request headers on the forward request for header copying
	forward.httpMethod = "POST"

	// Create request body
	req.Request = &extProcPb.ProcessingRequest_RequestBody{
		RequestBody: &extProcPb.HttpBody{
			Body: []byte(requestBody),
		},
	}

	ctx := context.Background()

	// Test non-streaming forwarding - pass reqHeaders instead to provide header access
	resp := server.forwardRequest(ctx, "test-request-1", reqHeaders, forward, false)

	// Verify response
	assert.NotNil(t, resp)
	immediateResp := resp.GetImmediateResponse()
	require.NotNil(t, immediateResp)

	assert.Equal(t, envoyTypePb.StatusCode_OK, immediateResp.GetStatus().GetCode())
	assert.Equal(t, `{"result": "success"}`, immediateResp.GetBody())

	// Check headers contain Content-Type and status
	headerOptions := immediateResp.GetHeaders().GetSetHeaders()
	foundContentType := false
	foundStatus := false

	for _, headerOpt := range headerOptions {
		header := headerOpt.GetHeader()
		if header.GetKey() == "Content-Type" {
			foundContentType = true
			assert.Equal(t, "application/json", header.GetValue())
		}
		if header.GetKey() == HeaderStatus {
			foundStatus = true
			assert.Equal(t, "200", header.GetValue())
		}
	}

	assert.True(t, foundContentType, "Content-Type header should be present")
	assert.True(t, foundStatus, "Status header should be present")
}

func TestForwardRequest_UploadStreaming(t *testing.T) {
	// Test the continueForward method directly for upload streaming
	server := &Server{}

	// Create forward request
	forward := &forwardingRequest{
		requestID: "test-streaming-upload",
		phase:     forwarding,
	}

	// Create pipe for testing - start a reader goroutine to prevent blocking
	reader, writer := io.Pipe()
	forward.writer = writer

	// Start reader goroutine to consume data
	readData := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		readData <- data
		_ = reader.Close()
	}()

	// Test data
	testData := "multipart form data chunk"

	// Test first chunk
	req1 := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body:        []byte(testData[:10]),
				EndOfStream: false,
			},
		},
	}

	ctx := context.Background()

	resp1 := server.continueForward(ctx, "test-streaming-upload", req1, forward)
	assert.NotNil(t, resp1)
	assert.Equal(t, forwardContinuing, forward.phase)

	// Test final chunk
	req2 := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body:        []byte(testData[10:]),
				EndOfStream: true,
			},
		},
	}

	resp2 := server.continueForward(ctx, "test-streaming-upload", req2, forward)
	assert.NotNil(t, resp2)
	assert.Equal(t, forwarded, forward.phase)
	assert.Nil(t, forward.writer) // Should be closed

	// Verify data was written correctly
	receivedData := <-readData
	assert.Equal(t, testData, string(receivedData))
}

func TestForwardRequest_DownloadStreaming(t *testing.T) {
	// Create test server that serves file for download
	// Use a smaller, more predictable content pattern for better debugging
	largeContent := strings.Repeat("CHUNK", 2000) // 10KB, predictable pattern
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=test.bin")
		w.Header().Set("Content-Length", strconv.Itoa(len(largeContent)))
		w.WriteHeader(http.StatusOK)

		// Write in smaller chunks to simulate real streaming behavior
		data := []byte(largeContent)
		chunkSize := 1024
		for i := 0; i < len(data); i += chunkSize {
			end := i + chunkSize
			if end > len(data) {
				end = len(data)
			}
			_, _ = w.Write(data[i:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Small delay to simulate network conditions
			time.Sleep(1 * time.Millisecond)
		}
	}))
	defer testServer.Close()

	// Create server instance
	server := &Server{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}

	// Create forward request
	forward := &forwardingRequest{
		requestID:   "test-download-streaming",
		targetURL:   testServer.URL,
		httpMethod:  "GET",
		contentType: "application/json",
		phase:       forwardPreparing,
	}

	// Create GET request (no body)
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{
						{Key: "Accept", Value: "*/*"},
					},
				},
			},
		},
	}

	ctx := context.Background()

	// Test download streaming - this should return CONTINUE_AND_REPLACE
	resp := server.forwardRequest(ctx, "test-download-streaming", req, forward, false)

	// Should get CONTINUE_AND_REPLACE response to start streaming
	assert.NotNil(t, resp)
	requestHeaders := resp.GetRequestHeaders()
	require.NotNil(t, requestHeaders)

	commonResp := requestHeaders.GetResponse()
	require.NotNil(t, commonResp)
	assert.Equal(t, extProcPb.CommonResponse_CONTINUE_AND_REPLACE, commonResp.GetStatus())

	// Verify forward request is properly configured for streaming
	assert.NotNil(t, forward.responseChan)
	assert.Equal(t, forwardRespondInitiated, forward.phase)

	// Wait a moment for the streaming goroutine to start
	time.Sleep(50 * time.Millisecond)

	// Collect all streaming responses
	var responses []*extProcPb.ProcessingResponse
	timeout := time.After(10 * time.Second) // Increase timeout

	// Collect all responses from the channel until it's closed or we timeout
	for {
		select {
		case resp, ok := <-forward.responseChan:
			if !ok {
				// Channel is closed, we got all responses
				goto processResponses
			}
			responses = append(responses, resp)
		case <-timeout:
			// Timeout - log and break out
			t.Logf("Timeout waiting for responses, got %d responses", len(responses))
			goto processResponses
		}
	}

processResponses:
	// We should have at least 2 responses: headers + at least one body chunk/trailer
	assert.GreaterOrEqual(t, len(responses), 2, "Should have headers and body/trailer responses")

	// First response should be headers
	resp2 := responses[0]
	assert.NotNil(t, resp2.GetResponseHeaders())
	assert.NotNil(t, resp2.GetResponseHeaders().GetResponse())
	assert.NotNil(t, resp2.GetResponseHeaders().GetResponse().GetHeaderMutation())
	headerOptions := resp2.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
	assert.NotNil(t, headerOptions)
	foundContentType := false
	foundContentDisposition := false
	foundStatus := false
	foundContentLength := false
	for _, headerOpt := range headerOptions {
		header := headerOpt.GetHeader()
		switch header.GetKey() {
		case "Content-Type":
			foundContentType = true
			assert.Equal(t, "application/octet-stream", header.GetValue())
		case "Content-Disposition":
			foundContentDisposition = true
			assert.Equal(t, "attachment; filename=test.bin", header.GetValue())
		case HeaderStatus:
			foundStatus = true
			assert.Equal(t, "200", header.GetValue())
		case "Content-Length":
			foundContentLength = true
			assert.Equal(t, strconv.Itoa(len(largeContent)), header.GetValue())
		}
	}
	assert.True(t, foundContentType, "Content-Type header should be present")
	assert.True(t, foundContentDisposition, "Content-Disposition header should be present")
	assert.True(t, foundStatus, "Status header should be present")
	assert.True(t, foundContentLength, "Content-Length header should be present")

	// Process remaining responses (body chunks and trailer)
	var receivedBytes []byte
	trailerSet := false
	for i := 1; i < len(responses); i++ {
		resp3 := responses[i]
		assert.False(t, trailerSet, "Content should not be set after trailer")
		assert.NotNil(t, resp3.Response)
		switch r := resp3.Response.(type) {
		case *extProcPb.ProcessingResponse_ResponseTrailers:
			trailerSet = true
			assert.NotNil(t, r.ResponseTrailers)
		case *extProcPb.ProcessingResponse_ResponseBody:
			assert.NotNil(t, r.ResponseBody)
			assert.NotNil(t, r.ResponseBody.Response)
			assert.NotNil(t, r.ResponseBody.Response.BodyMutation)
			partBody := r.ResponseBody.Response.BodyMutation.GetBody()
			assert.NotNil(t, partBody)

			// Use byte slice to avoid potential string conversion issues
			receivedBytes = append(receivedBytes, partBody...)
		}
	}

	// Check if we got all the content correctly
	expectedBytes := []byte(largeContent)

	t.Logf("Expected length: %d, Received length: %d", len(expectedBytes), len(receivedBytes))

	// Verify we received the exact content
	assert.Equal(t, len(expectedBytes), len(receivedBytes), "Content length should match exactly")
	assert.Equal(t, expectedBytes, receivedBytes, "Content should match exactly")

	// Optional trailer check - this is nice to have but not critical
	if trailerSet {
		t.Log("Trailer was properly set to indicate end of stream")
	} else {
		t.Log("No explicit trailer was set (this may be normal if channel was closed)")
	}
}

func TestForwardRequest_ErrorHandling(t *testing.T) {
	// Test with invalid URL
	server := &Server{
		httpClient: &http.Client{Timeout: 1 * time.Second},
	}

	forward := &forwardingRequest{
		requestID:   "test-error",
		targetURL:   "http://invalid-url-that-does-not-exist:99999",
		httpMethod:  "POST",
		contentType: "application/json",
		phase:       forwardPreparing,
	}

	// Create request with headers for the forward method to use
	headers := []*configPb.HeaderValue{
		{Key: "Content-Type", Value: "application/json"},
	}
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: headers,
				},
			},
		},
	}

	ctx := context.Background()

	// This should return an error response
	resp := server.forwardRequest(ctx, "test-error", req, forward, false)

	assert.NotNil(t, resp)
	immediateResp := resp.GetImmediateResponse()
	require.NotNil(t, immediateResp)

	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, immediateResp.GetStatus().GetCode())
	assert.Contains(t, immediateResp.GetBody(), "extend api server unavailable")

	// Check error header
	headerOptions := immediateResp.GetHeaders().GetSetHeaders()
	foundErrorHeader := false
	for _, headerOpt := range headerOptions {
		header := headerOpt.GetHeader()
		if header.GetKey() == HeaderErrorExtendAPI {
			foundErrorHeader = true
			assert.Equal(t, "true", string(header.GetRawValue()))
			break
		}
	}
	assert.True(t, foundErrorHeader, "Error header should be present")
}

func TestShouldStreamResponse(t *testing.T) {
	server := &Server{}

	tests := []struct {
		name           string
		statusCode     int
		contentType    string
		contentLength  int64
		expectedStream bool
	}{
		{
			name:           "small JSON response",
			statusCode:     http.StatusOK,
			contentType:    "application/json",
			contentLength:  100,
			expectedStream: false,
		},
		{
			name:           "large JSON response",
			statusCode:     http.StatusOK,
			contentType:    "application/json",
			contentLength:  2 * 1024 * 1024, // 2MB
			expectedStream: true,
		},
		{
			name:           "PDF file",
			statusCode:     http.StatusOK,
			contentType:    "application/pdf",
			contentLength:  500,
			expectedStream: true,
		},
		{
			name:           "image file",
			statusCode:     http.StatusOK,
			contentType:    "image/png",
			contentLength:  100,
			expectedStream: true,
		},
		{
			name:           "binary file",
			statusCode:     http.StatusOK,
			contentType:    "application/octet-stream",
			contentLength:  100,
			expectedStream: true,
		},
		{
			name:           "error response",
			statusCode:     http.StatusInternalServerError,
			contentType:    "application/json",
			contentLength:  100,
			expectedStream: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode:    tt.statusCode,
				ContentLength: tt.contentLength,
				Header:        make(http.Header),
			}
			resp.Header.Set("Content-Type", tt.contentType)

			result := server.shouldStreamResponse(resp)
			assert.Equal(t, tt.expectedStream, result)
		})
	}
}

func TestCopyResponseHeadersAndStatus(t *testing.T) {
	server := &Server{}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
	}
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Cache-Control", "no-cache")
	resp.Header.Add("Set-Cookie", "session=abc123")
	resp.Header.Add("Set-Cookie", "lang=en")

	headers := server.copyResponseHeadersAndStatus(resp)

	// Should have status + content-type + cache-control + 2 set-cookie headers = 5 total
	assert.Len(t, headers, 5)

	// Check status header is first
	assert.Equal(t, HeaderStatus, headers[0].GetHeader().GetKey())
	assert.Equal(t, "200", headers[0].GetHeader().GetValue())

	// Verify other headers are present
	headerMap := make(map[string][]string)
	for _, headerOpt := range headers {
		header := headerOpt.GetHeader()
		headerMap[header.GetKey()] = append(headerMap[header.GetKey()], header.GetValue())
	}

	assert.Equal(t, []string{"200"}, headerMap[HeaderStatus])
	assert.Equal(t, []string{"application/json"}, headerMap["Content-Type"])
	assert.Equal(t, []string{"no-cache"}, headerMap["Cache-Control"])
	assert.Equal(t, []string{"session=abc123", "lang=en"}, headerMap["Set-Cookie"])
}

func TestContinueForward(t *testing.T) {
	server := &Server{}

	// Create pipe for testing
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()

	forward := &forwardingRequest{
		requestID: "test-continue",
		writer:    writer,
		phase:     forwarding,
	}

	// Test intermediate chunk
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body:        []byte("test chunk data"),
				EndOfStream: false,
			},
		},
	}

	ctx := context.Background()
	resp := server.continueForward(ctx, "test-continue", req, forward)

	// Should return intermediate response
	assert.NotNil(t, resp)
	bodyResp := resp.GetRequestBody()
	require.NotNil(t, bodyResp)
	assert.NotNil(t, bodyResp.GetResponse())
	assert.Equal(t, forwardContinuing, forward.phase)

	// Test final chunk
	finalReq := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body:        []byte("final chunk"),
				EndOfStream: true,
			},
		},
	}

	finalResp := server.continueForward(ctx, "test-continue", finalReq, forward)

	// Should return intermediate response and close writer
	assert.NotNil(t, finalResp)
	assert.Equal(t, forwarded, forward.phase)
	assert.Nil(t, forward.writer) // Should be closed
}

func TestActiveForwardsContinuedRequest(t *testing.T) {
	server := &Server{
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	// Create forward request and store it
	forward := &forwardingRequest{
		requestID:   "continued-request",
		targetURL:   "http://example.com/api",
		httpMethod:  "POST",
		contentType: "multipart/form-data",
		phase:       forwarding,
	}

	// Create pipe to simulate active streaming
	_, writer := io.Pipe()
	forward.writer = writer

	server.activeForwards.Store("continued-request", forward)

	// Create continued request
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body:        []byte("continued data"),
				EndOfStream: false,
			},
		},
	}

	ctx := context.Background()

	// This should be detected as a continued request and handled differently
	resp, model, routingCtx, stream, term := server.HandleRequestBody(
		ctx, "continued-request", "/v1/files", req, utils.User{Name: "test"}, "")

	// Should get continue response
	assert.NotNil(t, resp)
	assert.Empty(t, model)
	assert.Nil(t, routingCtx)
	assert.False(t, stream)
	assert.Equal(t, int64(0), term)

	// Should be processed as continued forward
	bodyResp := resp.GetRequestBody()
	require.NotNil(t, bodyResp)
	assert.NotNil(t, bodyResp.GetResponse())

	// Clean up
	_ = writer.Close()
	server.activeForwards.Delete("continued-request")
}
