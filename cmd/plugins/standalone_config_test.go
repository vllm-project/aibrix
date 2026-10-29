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

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway"
)

// TestStandaloneConfigProfiles wires local mode the way main does: the static
// endpoints file feeds the cache, and the gateway server routes a request for
// the model. The routing strategy must come from the model's config field in
// the endpoints file, with the config-profile header picking a named profile.
func TestStandaloneConfigProfiles(t *testing.T) {
	endpoints := filepath.Join(t.TempDir(), "endpoints.yaml")
	require.NoError(t, os.WriteFile(endpoints, []byte(`
models:
  - name: local-model
    workers: ["127.0.0.1:1"]
    config: |
      {"defaultProfile": "default",
       "profiles": {"default": {"routingStrategy": "least-request"},
                    "cache-friendly": {"routingStrategy": "prefix-cache"}}}
`), 0o600))

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	// InitWithOptions sets up the process-wide cache once per test binary, so
	// this must stay the only test in this package that calls it.
	store := cache.InitWithOptions(nil, stopCh, cache.InitOptions{
		IsGateway:         true,
		DiscoveryProvider: discovery.NewStaticProvider(endpoints),
	})
	server := gateway.NewServerWithOptions(nil, nil, nil, gateway.ServerOptions{Cache: store})
	t.Cleanup(server.Shutdown)

	tests := []struct {
		name          string
		configProfile string
		wantStrategy  string
	}{
		{name: "default profile", wantStrategy: "least-request"},
		{name: "config-profile header", configProfile: "cache-friendly", wantStrategy: "prefix-cache"},
		{name: "unknown profile falls back to the default", configProfile: "missing", wantStrategy: "least-request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			headers := []*configPb.HeaderValue{{Key: ":path", RawValue: []byte("/v1/chat/completions")}}
			if tt.configProfile != "" {
				headers = append(headers, &configPb.HeaderValue{Key: "config-profile", RawValue: []byte(tt.configProfile)})
			}
			_, user, _, routingCtx, _ := server.HandleRequestHeaders(ctx, tt.name, trace.SpanFromContext(ctx),
				&extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{Headers: &configPb.HeaderMap{Headers: headers}},
				}})
			require.NotNil(t, routingCtx)

			resp, model, _, _ := server.HandleRequestBody(ctx, routingCtx, tt.name,
				&extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestBody{
					RequestBody: &extProcPb.HttpBody{
						Body: []byte(`{"model":"local-model","messages":[{"role":"user","content":"hello"}]}`),
					},
				}}, user)
			assert.Equal(t, "local-model", model)
			assert.Nil(t, resp.GetImmediateResponse())
			assert.Equal(t, tt.wantStrategy, string(routingCtx.Algorithm))
		})
	}
}
