/*
Copyright 2025 The Aibrix Team.

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

package tokenizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"k8s.io/klog/v2"
)

const (
	// Backoff configuration
	backoffBaseDelayMs = 100
	backoffMaxDelayMs  = 5000

	// HTTP transport configuration
	maxIdleConns          = 100
	maxIdleConnsPerHost   = 10
	idleConnTimeout       = 90 * time.Second
	dialTimeout           = 30 * time.Second
	keepAliveTimeout      = 30 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// httpClient handles HTTP requests to remote tokenizer services
type httpClient struct {
	baseURL    string
	httpClient *http.Client
	timeout    time.Duration
	maxRetries int
}

// retryableStatusCodes defines which HTTP status codes should trigger a retry
var retryableStatusCodes = map[int]bool{
	http.StatusRequestTimeout:      true, // 408
	http.StatusTooManyRequests:     true, // 429
	http.StatusInternalServerError: true, // 500
	http.StatusBadGateway:          true, // 502
	http.StatusServiceUnavailable:  true, // 503
	http.StatusGatewayTimeout:      true, // 504
}

// isRetryable determines if an HTTP response should be retried
func isRetryable(resp *http.Response) bool {
	return retryableStatusCodes[resp.StatusCode]
}

// getRetryDelay calculates the retry delay based on response headers and attempt number
func getRetryDelay(resp *http.Response, attempt int) time.Duration {
	// Check for Retry-After header
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		// Try to parse as seconds (integer)
		if seconds, err := strconv.Atoi(retryAfter); err == nil {
			return time.Duration(seconds) * time.Second
		}
		// Try to parse as HTTP date
		if t, err := http.ParseTime(retryAfter); err == nil {
			return time.Until(t)
		}
	}

	// Fall back to exponential backoff
	return calculateBackoff(attempt)
}

// calculateBackoff calculates exponential backoff delay
func calculateBackoff(attempt int) time.Duration {
	// Exponential backoff: 2^(attempt-1) * 100ms with max of 5 seconds
	delayMs := backoffBaseDelayMs << (attempt - 1)
	if delayMs > backoffMaxDelayMs {
		delayMs = backoffMaxDelayMs
	}
	return time.Duration(delayMs) * time.Millisecond
}

// isSuccess checks if the status code indicates success (2xx)
func isSuccess(statusCode int) bool {
	return statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices
}

// isRedirect checks if the status code indicates a redirect (3xx)
func isRedirect(statusCode int) bool {
	return statusCode >= http.StatusMultipleChoices && statusCode < http.StatusBadRequest
}

// isClientError checks if the status code indicates a client error (4xx)
func isClientError(statusCode int) bool {
	return statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError
}

// isServerError checks if the status code indicates a server error (5xx)
func isServerError(statusCode int) bool {
	return statusCode >= http.StatusInternalServerError
}

// newHTTPClient creates a new HTTP client for remote tokenizer requests
func newHTTPClient(baseURL string, timeout time.Duration, maxRetries int) *httpClient {
	// Create HTTP client with connection pooling
	transport := &http.Transport{
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout:     idleConnTimeout,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: keepAliveTimeout,
		}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		DisableCompression:    true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	return &httpClient{
		baseURL:    baseURL,
		httpClient: client,
		timeout:    timeout,
		maxRetries: maxRetries,
	}
}

// Post sends a POST request with retry logic
func (c *httpClient) Post(ctx context.Context, path string, request interface{}) ([]byte, error) {
	url := c.baseURL + path

	// Marshal request to JSON
	jsonData, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			// Calculate backoff delay
			backoff := calculateBackoff(attempt)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// Create new request for each attempt
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")

		// Send request
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			continue
		}

		// Read response body and ensure it's closed
		body, err := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if closeErr != nil {
			klog.V(2).InfoS("Failed to close response body", "error", closeErr)
		}

		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			continue
		}

		// Handle status code
		switch {
		case isSuccess(resp.StatusCode):
			// 2xx - Success
			return body, nil

		case isRedirect(resp.StatusCode):
			// 3xx - Redirect (treat as error, HTTP client should handle redirects automatically)
			return nil, ErrHTTPRequest{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("unexpected redirect on %s", path),
				Body:       string(body),
			}

		case resp.StatusCode == http.StatusTooManyRequests:
			// 429 - Too Many Requests (special handling with Retry-After)
			if attempt < c.maxRetries {
				delay := getRetryDelay(resp, attempt+1)
				select {
				case <-time.After(delay):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			lastErr = ErrHTTPRequest{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("rate limit exceeded on %s", path),
				Body:       string(body),
			}

		case isClientError(resp.StatusCode):
			// 4xx - Client error (non-retryable except for specific codes)
			if isRetryable(resp) {
				lastErr = ErrHTTPRequest{
					StatusCode: resp.StatusCode,
					Message:    fmt.Sprintf("retryable client error on %s", path),
					Body:       string(body),
				}
				continue
			}
			// Non-retryable client error
			return nil, ErrHTTPRequest{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("client error on %s", path),
				Body:       string(body),
			}

		case isServerError(resp.StatusCode):
			// 5xx - Server error (check if retryable)
			if isRetryable(resp) {
				lastErr = ErrHTTPRequest{
					StatusCode: resp.StatusCode,
					Message:    fmt.Sprintf("server error on %s", path),
					Body:       string(body),
				}
				continue
			}
			// Non-retryable server error
			return nil, ErrHTTPRequest{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("non-retryable server error on %s", path),
				Body:       string(body),
			}

		default:
			// Unknown status code
			return nil, ErrHTTPRequest{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("unexpected status code on %s", path),
				Body:       string(body),
			}
		}
	}

	return nil, fmt.Errorf("request failed after %d attempts: %w", c.maxRetries+1, lastErr)
}

// Get sends a GET request (useful for health checks)
func (c *httpClient) Get(ctx context.Context, path string) ([]byte, error) {
	url := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			klog.V(2).InfoS("Failed to close response body", "error", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if isSuccess(resp.StatusCode) {
		return body, nil
	}

	// Determine error message based on status code category
	var message string
	switch {
	case isRedirect(resp.StatusCode):
		message = fmt.Sprintf("unexpected redirect on GET %s", path)
	case isClientError(resp.StatusCode):
		message = fmt.Sprintf("client error on GET %s", path)
	case isServerError(resp.StatusCode):
		message = fmt.Sprintf("server error on GET %s", path)
	default:
		message = fmt.Sprintf("unexpected status code on GET %s", path)
	}

	return nil, ErrHTTPRequest{
		StatusCode: resp.StatusCode,
		Message:    message,
		Body:       string(body),
	}
}

// Close closes the HTTP client connections
func (c *httpClient) Close() {
	c.httpClient.CloseIdleConnections()
}
