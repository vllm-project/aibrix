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

// Package runpod is a self-contained client for the RunPod REST API
// (https://rest.runpod.io/v1). Like the lambdacloud package it is a leaf
// package: it imports only the standard library and MUST NOT import any
// resource_manager package.
package runpod

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
	// DefaultBaseURL is the RunPod REST API root.
	DefaultBaseURL = "https://rest.runpod.io/v1"

	defaultTimeout = 30 * time.Second

	envAPIKey       = "RUNPOD_API_KEY"
	envBaseURL      = "RUNPOD_BASE_URL"
	envDataCenters  = "RUNPOD_DATA_CENTERS" // comma-separated
	envImage        = "RUNPOD_IMAGE"
	envCloudType    = "RUNPOD_CLOUD_TYPE" // SECURE | COMMUNITY
	envSSHPublicKey = "RUNPOD_SSH_PUBLIC_KEY"

	// desiredStatus values returned by the API.
	StatusRunning    = "RUNNING"
	StatusExited     = "EXITED"
	StatusTerminated = "TERMINATED"

	CloudTypeSecure    = "SECURE"
	CloudTypeCommunity = "COMMUNITY"
)

// Config holds credentials and launch defaults for a RunPod account. APIKey is
// the only required field; the rest provide pod launch defaults.
type Config struct {
	APIKey            string
	BaseURL           string
	DataCenterIds     []string
	ImageName         string
	CloudType         string
	SSHPublicKey      string // operator public key injected for inbound SSH
	Ports             []string
	ContainerDiskInGb int
	VolumeInGb        int
	HTTPClient        *http.Client
}

// ConfigFromEnv builds a service-level Config from environment variables. It
// always returns a non-nil Config; callers should check APIKey before use.
func ConfigFromEnv() *Config {
	cfg := &Config{
		APIKey:    os.Getenv(envAPIKey),
		BaseURL:   os.Getenv(envBaseURL),
		ImageName: os.Getenv(envImage),
		CloudType: os.Getenv(envCloudType),
	}
	cfg.SSHPublicKey = os.Getenv(envSSHPublicKey)
	if raw := os.Getenv(envDataCenters); raw != "" {
		for _, dc := range strings.Split(raw, ",") {
			if dc = strings.TrimSpace(dc); dc != "" {
				cfg.DataCenterIds = append(cfg.DataCenterIds, dc)
			}
		}
	}
	return cfg
}

// Client is a thin RunPod REST client.
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
// API DTOs (subset of the RunPod schema we consume)
// ============================================================================

// PodCreateInput is the body of POST /pods.
type PodCreateInput struct {
	Name              string            `json:"name,omitempty"`
	ImageName         string            `json:"imageName,omitempty"`
	ComputeType       string            `json:"computeType,omitempty"`
	CloudType         string            `json:"cloudType,omitempty"`
	GpuTypeIds        []string          `json:"gpuTypeIds,omitempty"`
	GpuCount          int               `json:"gpuCount,omitempty"`
	GpuTypePriority   string            `json:"gpuTypePriority,omitempty"`
	DataCenterIds     []string          `json:"dataCenterIds,omitempty"`
	ContainerDiskInGb int               `json:"containerDiskInGb,omitempty"`
	VolumeInGb        int               `json:"volumeInGb,omitempty"`
	Ports             []string          `json:"ports,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	// DockerStartCmd overrides the image CMD so the server does NOT auto-start
	// (the container runs sshd + idles; MDS launches vLLM over SSH).
	DockerStartCmd []string `json:"dockerStartCmd,omitempty"`
	// DockerEntrypoint overrides the image ENTRYPOINT when the base image's
	// entrypoint would still launch/alter the server.
	DockerEntrypoint []string `json:"dockerEntrypoint,omitempty"`
	SupportPublicIp  *bool    `json:"supportPublicIp,omitempty"`
}

// Machine is the subset of a pod's machine details we read.
type Machine struct {
	GpuTypeId    string `json:"gpuTypeId"`
	DataCenterId string `json:"dataCenterId"`
	Location     string `json:"location"`
}

// Pod is the response object for create/get.
type Pod struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	DesiredStatus string         `json:"desiredStatus"`
	Image         string         `json:"image"`
	PublicIp      string         `json:"publicIp"`
	PortMappings  map[string]int `json:"portMappings"`
	MachineId     string         `json:"machineId"`
	Machine       Machine        `json:"machine"`
	CostPerHr     float64        `json:"costPerHr"`
}

// APIError is the structured error returned by the RunPod API.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("runpod api %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("runpod api %d: %s", e.Status, e.Message)
}

// ============================================================================
// Operations
// ============================================================================

// CreatePod launches a single pod and returns it.
func (c *Client) CreatePod(ctx context.Context, input PodCreateInput) (*Pod, error) {
	var pod Pod
	if err := c.do(ctx, http.MethodPost, "/pods", input, &pod); err != nil {
		return nil, err
	}
	return &pod, nil
}

// GetPod fetches a pod by ID.
func (c *Client) GetPod(ctx context.Context, id string) (*Pod, error) {
	var pod Pod
	if err := c.do(ctx, http.MethodGet, "/pods/"+id, nil, &pod); err != nil {
		return nil, err
	}
	return &pod, nil
}

// DeletePod terminates a pod by ID. A 404 is treated as already-gone.
func (c *Client) DeletePod(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/pods/"+id, nil, nil)
	if apiErr, ok := err.(*APIError); ok && apiErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// do performs an authenticated JSON request with Bearer auth.
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
		if json.Unmarshal(payload, apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(payload))
		}
		return apiErr
	}

	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
