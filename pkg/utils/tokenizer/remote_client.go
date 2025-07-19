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

package tokenizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

// HTTPClient handles HTTP requests to remote tokenizer services
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
	config     HTTPClientConfig
}

// NewHTTPClient creates a new HTTP client for remote tokenizer requests
func NewHTTPClient(baseURL string, config HTTPClientConfig) *HTTPClient {
	// Create HTTP client with connection pooling
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   config.Timeout,
	}

	return &HTTPClient{
		baseURL:    baseURL,
		httpClient: httpClient,
		config:     config,
	}
}

// Post sends a POST request with retry logic
func (c *HTTPClient) Post(ctx context.Context, path string, request interface{}) ([]byte, error) {
	url := c.baseURL + path

	// Marshal request to JSON
	jsonData, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 2^attempt * 100ms with max of 5 seconds
			backoff := time.Duration(math.Min(float64(int(1)<<uint(attempt-1))*100, 5000)) * time.Millisecond
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

		// Check status code
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, nil
		}

		// Handle different error codes
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// Client errors (4xx) - don't retry
			return nil, ErrHTTPRequest{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("client error on %s", path),
				Body:       string(body),
			}
		}

		// Server errors (5xx) - retry
		lastErr = ErrHTTPRequest{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("server error on %s", path),
			Body:       string(body),
		}
	}

	return nil, fmt.Errorf("request failed after %d attempts: %w", c.config.MaxRetries+1, lastErr)
}

// Get sends a GET request (useful for health checks)
func (c *HTTPClient) Get(ctx context.Context, path string) ([]byte, error) {
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

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}

	return nil, ErrHTTPRequest{
		StatusCode: resp.StatusCode,
		Message:    fmt.Sprintf("GET request failed on %s", path),
		Body:       string(body),
	}
}

// Close closes the HTTP client connections
func (c *HTTPClient) Close() {
	c.httpClient.CloseIdleConnections()
}
