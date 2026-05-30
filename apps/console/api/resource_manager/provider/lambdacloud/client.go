/*
Copyright 2026 The Aibrix Team.

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

// Package lambdacloud is a self-contained client for the Lambda Cloud public
// API (https://docs.lambda.ai/public-cloud/cloud-api/). It is intentionally a
// leaf package: it imports only the standard library and MUST NOT import any
// resource_manager package, so resource_manager/types can embed *Client into a
// clientset without an import cycle.
package lambdacloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the Lambda Cloud API root.
	DefaultBaseURL = "https://cloud.lambda.ai/api/v1"

	defaultTimeout = 30 * time.Second

	// Env var names for the service-level (operator-configured) account.
	envAPIKey  = "LAMBDA_CLOUD_API_KEY"
	envBaseURL = "LAMBDA_CLOUD_BASE_URL"
	envSSHKeys = "LAMBDA_CLOUD_SSH_KEYS" // comma-separated
	envRegion  = "LAMBDA_CLOUD_REGION"

	// Instance lifecycle states reported by the Lambda API.
	StatusBooting     = "booting"
	StatusActive      = "active"
	StatusUnhealthy   = "unhealthy"
	StatusTerminating = "terminating"
	StatusTerminated  = "terminated"
)

// Config holds the credentials and defaults used to talk to a Lambda Cloud
// account. APIKey is the only required field.
type Config struct {
	APIKey      string
	BaseURL     string
	SSHKeyNames []string
	Region      string
	HTTPClient  *http.Client
}

// ConfigFromEnv builds a service-level Config from environment variables. It
// always returns a non-nil Config; callers should check APIKey before use.
func ConfigFromEnv() *Config {
	cfg := &Config{
		APIKey:  os.Getenv(envAPIKey),
		BaseURL: os.Getenv(envBaseURL),
		Region:  os.Getenv(envRegion),
	}
	if raw := os.Getenv(envSSHKeys); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				cfg.SSHKeyNames = append(cfg.SSHKeyNames, name)
			}
		}
	}
	return cfg
}

// Client is a thin Lambda Cloud REST client.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewClient builds a Client from cfg. APIKey must be set.
func NewClient(cfg *Config) *Client {
	baseURL := DefaultBaseURL
	httpClient := &http.Client{Timeout: defaultTimeout}
	apiKey := ""
	if cfg != nil {
		if cfg.BaseURL != "" {
			baseURL = strings.TrimRight(cfg.BaseURL, "/")
		}
		if cfg.HTTPClient != nil {
			httpClient = cfg.HTTPClient
		}
		apiKey = cfg.APIKey
	}
	return &Client{apiKey: apiKey, baseURL: baseURL, http: httpClient}
}

// ============================================================================
// API DTOs (subset of the Lambda Cloud schema we consume)
// ============================================================================

type Specs struct {
	VCPUs      int `json:"vcpus"`
	MemoryGiB  int `json:"memory_gib"`
	StorageGiB int `json:"storage_gib"`
	GPUs       int `json:"gpus"`
}

type InstanceType struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	GPUDescription    string `json:"gpu_description"`
	PriceCentsPerHour int    `json:"price_cents_per_hour"`
	Specs             Specs  `json:"specs"`
}

type Region struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// InstanceTypeEntry pairs an instance type with the regions that currently
// have capacity for it.
type InstanceTypeEntry struct {
	InstanceType                 InstanceType `json:"instance_type"`
	RegionsWithCapacityAvailable []Region     `json:"regions_with_capacity_available"`
}

type Instance struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	IP           string       `json:"ip"`
	PrivateIP    string       `json:"private_ip"`
	Status       string       `json:"status"`
	Region       Region       `json:"region"`
	InstanceType InstanceType `json:"instance_type"`
	SSHKeyNames  []string     `json:"ssh_key_names"`
}

// LaunchRequest is the body of POST /instance-operations/launch.
//
// Note: the current Lambda API launches a single instance per call (there is no
// "quantity" field); launch multiple times to provision multiple replicas.
// UserData (cloud-init) is available for baking workload startup into the boot.
type LaunchRequest struct {
	RegionName       string   `json:"region_name"`
	InstanceTypeName string   `json:"instance_type_name"`
	SSHKeyNames      []string `json:"ssh_key_names"`
	FileSystemNames  []string `json:"file_system_names,omitempty"`
	Name             string   `json:"name,omitempty"`
	UserData         string   `json:"user_data,omitempty"`
}

// APIError is the structured error returned by the Lambda API.
type APIError struct {
	Status     int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
}

func (e *APIError) Error() string {
	if e.Suggestion != "" {
		return fmt.Sprintf("lambda api %d %s: %s (%s)", e.Status, e.Code, e.Message, e.Suggestion)
	}
	return fmt.Sprintf("lambda api %d %s: %s", e.Status, e.Code, e.Message)
}

// ============================================================================
// Operations
// ============================================================================

// ListInstanceTypes returns the catalog keyed by instance type name.
func (c *Client) ListInstanceTypes(ctx context.Context) (map[string]InstanceTypeEntry, error) {
	var resp struct {
		Data map[string]InstanceTypeEntry `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/instance-types", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// LaunchInstances launches quantity instances and returns their IDs.
func (c *Client) LaunchInstances(ctx context.Context, req LaunchRequest) ([]string, error) {
	var resp struct {
		Data struct {
			InstanceIDs []string `json:"instance_ids"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/instance-operations/launch", req, &resp); err != nil {
		return nil, err
	}
	return resp.Data.InstanceIDs, nil
}

// GetInstance fetches a single instance by ID.
func (c *Client) GetInstance(ctx context.Context, id string) (*Instance, error) {
	var resp struct {
		Data Instance `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+id, nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// TerminateInstances terminates the given instance IDs.
func (c *Client) TerminateInstances(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	body := struct {
		InstanceIDs []string `json:"instance_ids"`
	}{InstanceIDs: ids}
	return c.do(ctx, http.MethodPost, "/instance-operations/terminate", body, nil)
}

// do performs an authenticated JSON request. The Lambda Cloud API uses HTTP
// Basic auth with the API key as the username and an empty password.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.SetBasicAuth(c.apiKey, "")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode}
		var wrapper struct {
			Error APIError `json:"error"`
		}
		if json.Unmarshal(payload, &wrapper) == nil && wrapper.Error.Message != "" {
			apiErr.Code = wrapper.Error.Code
			apiErr.Message = wrapper.Error.Message
			apiErr.Suggestion = wrapper.Error.Suggestion
		} else {
			apiErr.Message = strings.TrimSpace(string(payload))
		}
		return apiErr
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
