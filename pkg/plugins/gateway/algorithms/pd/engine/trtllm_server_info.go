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

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
	"golang.org/x/sync/singleflight"
	v1 "k8s.io/api/core/v1"
)

const (
	trtServerInfoTTL        = time.Minute
	trtServerInfoTimeout    = 3 * time.Second
	trtServerInfoMaxEntries = 1024
	trtServerInfoMaxBytes   = 1 << 20
)

// TRTServerInfo is the worker-scoped metadata used to bootstrap a generation
// request without waiting for the context response. DPRank is attention DP,
// not tensor parallel rank or a Kubernetes replica index.
type TRTServerInfo struct {
	ContextInfoEndpoint string `json:"ctx_info_endpoint"`
	ContextDPRank       int    `json:"ctx_dp_rank"`
	EncodedOpaqueState  string `json:"encoded_opaque_state,omitempty"`
}

func (info TRTServerInfo) validate() error {
	if strings.TrimSpace(info.ContextInfoEndpoint) == "" || info.ContextDPRank < 0 {
		return fmt.Errorf("TRT server_info requires a nonempty ctx_info_endpoint and nonnegative ctx_dp_rank")
	}
	return nil
}

// TRTServerInfoProvider resolves metadata for the exact selected context worker.
// Implementations must be safe for concurrent requests.
type TRTServerInfoProvider interface {
	Get(context.Context, *v1.Pod) (TRTServerInfo, error)
}

type trtServerInfoEntry struct {
	info    TRTServerInfo
	expires time.Time
}

// TRTServerInfoCache loads workers lazily, since pods need not exist when the
// router is constructed. Entries expire and are bounded; no background worker
// or informer lifecycle is required. Concurrent misses for one incarnation are
// coalesced, and a canceled waiter cannot cancel another request's lookup.
type TRTServerInfoCache struct {
	client  *http.Client
	mu      sync.Mutex
	entries map[string]trtServerInfoEntry
	loads   singleflight.Group
	now     func() time.Time
}

func NewTRTServerInfoCache(client *http.Client) *TRTServerInfoCache {
	// Reuse the transport, but never follow a worker's redirect to a different
	// endpoint. server_info must describe the worker we actually selected.
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &TRTServerInfoCache{
		client:  &copyClient,
		entries: make(map[string]trtServerInfoEntry),
		now:     time.Now,
	}
}

func (c *TRTServerInfoCache) Get(ctx context.Context, pod *v1.Pod) (TRTServerInfo, error) {
	if err := ctx.Err(); err != nil {
		return TRTServerInfo{}, err
	}
	if pod == nil || pod.Status.PodIP == "" {
		return TRTServerInfo{}, fmt.Errorf("TRT server_info requires a context pod IP")
	}
	addr := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(utils.GetModelPortForPod("", pod))))
	// UID prevents Pod-name/IP reuse; container identity catches in-place
	// restarts that retain both UID and IP but change the coordination endpoint.
	var key strings.Builder
	key.WriteString(string(pod.UID))
	key.WriteString("/")
	key.WriteString(addr)
	for _, status := range pod.Status.ContainerStatuses {
		fmt.Fprintf(&key, "/%s:%s:%d", status.Name, status.ContainerID, status.RestartCount)
	}
	if info, ok := c.lookup(key.String()); ok {
		return info, nil
	}
	result := c.loads.DoChan(key.String(), func() (any, error) {
		if info, ok := c.lookup(key.String()); ok {
			return info, nil
		}
		// Detach only the shared metadata lookup, never the inference request.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), trtServerInfoTimeout)
		defer cancel()
		info, err := c.fetch(fetchCtx, "http://"+addr+"/server_info")
		if err != nil {
			return TRTServerInfo{}, err
		}
		c.store(key.String(), info)
		return info, nil
	})
	select {
	case <-ctx.Done():
		return TRTServerInfo{}, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return TRTServerInfo{}, result.Err
		}
		return result.Val.(TRTServerInfo), nil
	}
}

func (c *TRTServerInfoCache) lookup(key string) (TRTServerInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if ok && c.now().Before(entry.expires) {
		return entry.info, true
	}
	delete(c.entries, key)
	return TRTServerInfo{}, false
}

func (c *TRTServerInfoCache) store(key string, info TRTServerInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var oldestKey string
	var oldest time.Time
	for k, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, k)
			continue
		}
		if oldest.IsZero() || entry.expires.Before(oldest) {
			oldestKey, oldest = k, entry.expires
		}
	}
	if len(c.entries) >= trtServerInfoMaxEntries {
		delete(c.entries, oldestKey)
	}
	c.entries[key] = trtServerInfoEntry{info: info, expires: now.Add(trtServerInfoTTL)}
}

func (c *TRTServerInfoCache) fetch(ctx context.Context, url string) (TRTServerInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return TRTServerInfo{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return TRTServerInfo{}, fmt.Errorf("fetch TRT server_info: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TRTServerInfo{}, fmt.Errorf("TRT server_info returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, trtServerInfoMaxBytes+1))
	if err != nil {
		return TRTServerInfo{}, fmt.Errorf("read TRT server_info: %w", err)
	}
	if len(body) > trtServerInfoMaxBytes {
		return TRTServerInfo{}, fmt.Errorf("TRT server_info exceeds %d bytes", trtServerInfoMaxBytes)
	}
	// A pointer distinguishes missing/null rank from the valid rank zero.
	var response struct {
		Params *struct {
			ContextInfoEndpoint json.RawMessage `json:"ctx_info_endpoint"`
			ContextDPRank       *int            `json:"ctx_dp_rank"`
			EncodedOpaqueState  string          `json:"encoded_opaque_state"`
		} `json:"disaggregated_params"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return TRTServerInfo{}, fmt.Errorf("decode TRT server_info: %w", err)
	}
	if response.Params == nil || response.Params.ContextDPRank == nil {
		// A worker whose KV-cache transceiver is the C++ one reports an empty
		// disaggregated_params: only the Python transceiver implements the
		// generation-first metadata. Name that likely cause, because the raw
		// symptom (a missing rank) does not point at the worker's config.
		return TRTServerInfo{}, fmt.Errorf("TRT server_info is missing disaggregated_params.ctx_dp_rank; " +
			"generation_first requires the worker's Python KV-cache transceiver " +
			"(cache_transceiver_config.transceiver_runtime: PYTHON, backend DEFAULT or NIXL)")
	}
	endpoint, err := decodeTRTEndpoint(response.Params.ContextInfoEndpoint)
	if err != nil {
		return TRTServerInfo{}, err
	}
	info := TRTServerInfo{
		ContextInfoEndpoint: endpoint,
		ContextDPRank:       *response.Params.ContextDPRank,
		EncodedOpaqueState:  response.Params.EncodedOpaqueState,
	}
	return info, info.validate()
}

// decodeTRTEndpoint reads ctx_info_endpoint from /server_info. The Python
// transceiver returns it as a single "tcp://ip:port" string, and that is the
// form handled first. A one-element array is accepted too, because the engine's
// own DisaggregatedParams dataclass declares the same logical field as a list
// of endpoints. A longer array is rejected rather than guessed at: the element
// belonging to the rank the gateway selected cannot be identified here, and
// picking the wrong one would hand generation a peer it cannot use.
func decodeTRTEndpoint(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var endpoint string
	if err := json.Unmarshal(raw, &endpoint); err == nil {
		return endpoint, nil
	}
	var endpoints []string
	if err := json.Unmarshal(raw, &endpoints); err != nil {
		return "", fmt.Errorf("TRT server_info ctx_info_endpoint is neither a string nor an array of strings")
	}
	switch len(endpoints) {
	case 1:
		return endpoints[0], nil
	case 0:
		return "", nil
	default:
		return "", fmt.Errorf("TRT server_info returned %d ctx_info_endpoint values; "+
			"a rank-affine single endpoint is required, so this worker needs one endpoint per rank", len(endpoints))
	}
}
