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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func int64Ptr(v int64) *int64 {
	return &v
}

const autoConfigProfile = "auto"

func TestApplyConfigProfile_AutoProfileRoutingConfigHints(t *testing.T) {
	profileJSON := `{
		"defaultProfile":"default",
		"profiles":{
			"default":{"routingStrategy":"least-request"},
			"large-input":{"routingStrategy":"pd","routingConfig":{"promptTokensGte":4},"requestsPerSecond":5},
			"offline-generation":{"routingStrategy":"throughput","routingConfig":{"maxTokensGte":2048}}
		}
	}`
	pods := []*v1.Pod{{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod-a",
			Annotations: map[string]string{constants.ModelAnnoConfig: profileJSON},
		},
	}}

	ctx := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
	ctx.ReqConfigProfile = autoConfigProfile
	ctx.Message = "one two three four"
	ctx.ReqBody = []byte(`{"max_tokens":128}`)
	ctx.ReqHeaders = map[string]string{}

	applyConfigProfile(ctx, pods)

	assert.NotNil(t, ctx.ConfigProfile)
	assert.Equal(t, "pd", ctx.ConfigProfile.RoutingStrategy)
	assert.Equal(t, int64(5), ctx.ConfigProfile.RequestsPerSecond)
	assert.Equal(t, "large-input", ctx.ReqConfigProfile)
	assert.Equal(t, "large-input", ctx.RespHeaders[HeaderAIBrixConfigProfile])
}

func TestApplyConfigProfile_AutoProfileRoutingConfigHintFallbacks(t *testing.T) {
	tests := []struct {
		name             string
		profileJSON      string
		reqHeaders       map[string]string
		reqBody          []byte
		wantStrategy     string
		wantReqProfile   string
		wantProfileHdr   string
		wantHeaderAbsent bool
	}{
		{
			name: "no matching hint uses default profile",
			profileJSON: `{
				"defaultProfile":"default",
				"profiles":{"default":{"routingStrategy":"least-request"},"throughput":{"routingStrategy":"throughput","routingConfig":{"maxTokensGte":2048}}}
			}`,
			reqHeaders:     map[string]string{},
			reqBody:        []byte(`{"max_tokens":128}`),
			wantStrategy:   "least-request",
			wantReqProfile: "default",
			wantProfileHdr: "default",
		},
		{
			name: "invalid routingConfig hint uses default profile",
			profileJSON: `{
				"defaultProfile":"default",
				"profiles":{"default":{"routingStrategy":"least-request"},"large-input":{"routingStrategy":"pd","routingConfig":"invalid"}}
			}`,
			reqHeaders:     map[string]string{},
			reqBody:        []byte(`{"max_tokens":128}`),
			wantStrategy:   "least-request",
			wantReqProfile: "default",
			wantProfileHdr: "default",
		},
		{
			name: "routing strategy header does not skip auto selection",
			profileJSON: `{
				"defaultProfile":"default",
				"profiles":{"default":{"routingStrategy":"least-request"},"large-input":{"routingStrategy":"pd","routingConfig":{"promptTokensGte":1},"requestsPerSecond":5}}
			}`,
			reqHeaders:     map[string]string{HeaderRoutingStrategy: "throughput"},
			reqBody:        []byte(`{"max_tokens":128}`),
			wantStrategy:   "pd",
			wantReqProfile: "large-input",
			wantProfileHdr: "large-input",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods := []*v1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "pod-a",
					Annotations: map[string]string{constants.ModelAnnoConfig: tt.profileJSON},
				},
			}}
			ctx := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
			ctx.ReqConfigProfile = autoConfigProfile
			ctx.Message = "prompt"
			ctx.ReqBody = tt.reqBody
			ctx.ReqHeaders = tt.reqHeaders

			applyConfigProfile(ctx, pods)

			assert.NotNil(t, ctx.ConfigProfile)
			assert.Equal(t, tt.wantStrategy, ctx.ConfigProfile.RoutingStrategy)
			assert.Equal(t, tt.wantReqProfile, ctx.ReqConfigProfile)
			if tt.wantHeaderAbsent {
				assert.NotContains(t, ctx.RespHeaders, HeaderAIBrixConfigProfile)
			} else {
				assert.Equal(t, tt.wantProfileHdr, ctx.RespHeaders[HeaderAIBrixConfigProfile])
			}
		})
	}
}

func TestApplyConfigProfile_BuildsFeaturesOnlyForAutoSelection(t *testing.T) {
	profileJSON := `{
		"defaultProfile":"default",
		"profiles":{
			"default":{"routingStrategy":"least-request"},
			"batch":{"routingStrategy":"throughput","routingConfig":{"promptTokensGte":1}}
		}
	}`
	pods := []*v1.Pod{{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod-a",
			Annotations: map[string]string{constants.ModelAnnoConfig: profileJSON},
		},
	}}

	tests := []struct {
		name             string
		reqConfigProfile string
		reqHeaders       map[string]string
		wantStrategy     string
		wantReqProfile   string
	}{
		{
			name:             "concrete profile skips request feature parsing",
			reqConfigProfile: "batch",
			reqHeaders:       map[string]string{},
			wantStrategy:     "throughput",
			wantReqProfile:   "batch",
		},
		{
			name:             "routing strategy header still resolves auto profile",
			reqConfigProfile: autoConfigProfile,
			reqHeaders:       map[string]string{HeaderRoutingStrategy: "throughput"},
			wantStrategy:     "throughput",
			wantReqProfile:   "batch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
			ctx.ReqConfigProfile = tt.reqConfigProfile
			ctx.ReqHeaders = tt.reqHeaders
			ctx.ReqBody = []byte(`{`)
			ctx.Message = "prompt"

			applyConfigProfile(ctx, pods)

			assert.NotNil(t, ctx.ConfigProfile)
			assert.Equal(t, tt.wantStrategy, ctx.ConfigProfile.RoutingStrategy)
			assert.Equal(t, tt.wantReqProfile, ctx.ReqConfigProfile)
			if tt.reqConfigProfile == autoConfigProfile {
				assert.Equal(t, tt.wantReqProfile, ctx.RespHeaders[HeaderAIBrixConfigProfile])
			} else {
				assert.NotContains(t, ctx.RespHeaders, HeaderAIBrixConfigProfile)
			}
		})
	}
}

func TestApplyConfigProfile_AuthoritativePolicyUsesDefaultProfile(t *testing.T) {
	profileJSON := `{
		"authoritativeRoutingPolicy":true,
		"defaultProfile":"default",
		"profiles":{
			"default":{"routingStrategy":"least-request","routingConfig":{"marker":"default"}},
			"batch":{"routingStrategy":"throughput","routingConfig":{"promptTokensGte":1,"marker":"batch"}}
		}
	}`
	pods := []*v1.Pod{{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod-a",
			Annotations: map[string]string{constants.ModelAnnoConfig: profileJSON},
		},
	}}

	for _, reqConfigProfile := range []string{"batch", autoConfigProfile} {
		t.Run(reqConfigProfile, func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
			ctx.ReqConfigProfile = reqConfigProfile
			ctx.Message = "prompt"
			ctx.ReqHeaders = map[string]string{
				HeaderRoutingStrategy: "throughput",
				HeaderExternalFilter:  "environment=batch",
				"x-test-header":       "preserved",
			}

			applyConfigProfile(ctx, pods)

			require.NotNil(t, ctx.ConfigProfile)
			assert.True(t, ctx.ConfigProfile.AuthoritativeRoutingPolicy)
			assert.Equal(t, "least-request", ctx.ConfigProfile.RoutingStrategy)
			assert.Contains(t, string(ctx.ConfigProfile.RoutingConfig), `"marker":"default"`)
			assert.Empty(t, ctx.ReqConfigProfile)
			assert.NotContains(t, ctx.ReqHeaders, HeaderRoutingStrategy)
			assert.NotContains(t, ctx.ReqHeaders, HeaderExternalFilter)
			assert.Equal(t, "preserved", ctx.ReqHeaders["x-test-header"])
			assert.NotContains(t, ctx.RespHeaders, HeaderAIBrixConfigProfile)
		})
	}
}

func TestApplyConfigProfile_ModelConfig(t *testing.T) {
	modelConfig := `{
		"defaultProfile":"default",
		"profiles":{
			"default":{"routingStrategy":"least-request"},
			"cache-friendly":{"routingStrategy":"prefix-cache","requestsPerSecond":7}
		}
	}`
	annotated := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pod-a",
		Annotations: map[string]string{
			constants.ModelAnnoConfig: `{"defaultProfile":"default","profiles":{"default":{"routingStrategy":"random"}}}`,
		},
	}}
	plain := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}}

	tests := []struct {
		name             string
		modelConfig      string
		reqConfigProfile string
		pods             []*v1.Pod
		wantStrategy     string
		wantRPS          int64
	}{
		{
			name:         "model config default profile",
			modelConfig:  modelConfig,
			pods:         []*v1.Pod{plain},
			wantStrategy: "least-request",
		},
		{
			name:             "config-profile picks a model config profile",
			modelConfig:      modelConfig,
			reqConfigProfile: "cache-friendly",
			pods:             []*v1.Pod{plain},
			wantStrategy:     "prefix-cache",
			wantRPS:          7,
		},
		{
			name:         "model config wins over a pod annotation",
			modelConfig:  modelConfig,
			pods:         []*v1.Pod{annotated},
			wantStrategy: "least-request",
		},
		{
			name:         "no model config keeps the pod annotation",
			pods:         []*v1.Pod{annotated},
			wantStrategy: "random",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), "", "model", "", "request-1", "")
			ctx.ModelConfig = tt.modelConfig
			ctx.ReqConfigProfile = tt.reqConfigProfile

			applyConfigProfile(ctx, tt.pods)

			require.NotNil(t, ctx.ConfigProfile)
			assert.Equal(t, tt.wantStrategy, ctx.ConfigProfile.RoutingStrategy)
			assert.Equal(t, tt.wantRPS, ctx.ConfigProfile.RequestsPerSecond)
		})
	}
}

// modelConfigTestCache adds cache.ModelConfigCache to MockCache.
type modelConfigTestCache struct {
	*MockCache
	configs map[string]string
}

func (c modelConfigTestCache) ModelConfig(model string) (string, bool) {
	cfg, ok := c.configs[model]
	return cfg, ok
}

func TestModelConfigFromCache(t *testing.T) {
	withConfigs := modelConfigTestCache{MockCache: &MockCache{}, configs: map[string]string{"model": `{"profiles":{}}`}}
	assert.Equal(t, `{"profiles":{}}`, modelConfigFromCache(withConfigs, "model"))
	assert.Empty(t, modelConfigFromCache(withConfigs, "other-model"))
	assert.Empty(t, modelConfigFromCache(&MockCache{}, "model"), "a cache without ModelConfigCache has no model config")
}

func TestMaxTokensFromRequestBody(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want *int64
	}{
		{name: "max tokens", body: []byte(`{"max_tokens":128}`), want: int64Ptr(128)},
		{name: "max completion tokens", body: []byte(`{"max_completion_tokens":256}`), want: int64Ptr(256)},
		{name: "max tokens wins", body: []byte(`{"max_tokens":128,"max_completion_tokens":256}`), want: int64Ptr(128)},
		{name: "absent", body: []byte(`{"model":"m"}`), want: nil},
		{name: "invalid", body: []byte(`{`), want: nil},
		{name: "null max tokens falls back", body: []byte(`{"max_tokens":null,"max_completion_tokens":256}`), want: int64Ptr(256)},
		{name: "null max tokens alone is absent", body: []byte(`{"max_tokens":null}`), want: nil},
		{name: "null max completion tokens is absent", body: []byte(`{"max_completion_tokens":null}`), want: nil},
		{name: "both max fields null is absent", body: []byte(`{"max_tokens":null,"max_completion_tokens":null}`), want: nil},
		{name: "null max completion tokens keeps max tokens", body: []byte(`{"max_tokens":128,"max_completion_tokens":null}`), want: int64Ptr(128)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maxTokensFromRequestBody(tt.body)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			if assert.NotNil(t, got) {
				assert.Equal(t, *tt.want, *got)
			}
		})
	}
}

func TestAutoConfigProfileIgnoresNullMaxTokens(t *testing.T) {
	profileJSON := `{
		"defaultProfile":"default",
		"profiles":{
			"default":{"routingStrategy":"least-request"},
			"short-output":{"routingStrategy":"throughput","routingConfig":{"maxTokensLt":4096}}
		}
	}`
	pods := []*v1.Pod{{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod-a",
			Annotations: map[string]string{constants.ModelAnnoConfig: profileJSON},
		},
	}}

	tests := []struct {
		name     string
		body     []byte
		wantName string
	}{
		{name: "null max tokens selects default", body: []byte(`{"max_tokens":null}`), wantName: "default"},
		{name: "null max tokens ignores large max completion tokens", body: []byte(`{"max_tokens":null,"max_completion_tokens":8192}`), wantName: "default"},
		{name: "null max tokens falls back to small max completion tokens", body: []byte(`{"max_tokens":null,"max_completion_tokens":256}`), wantName: "short-output"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
			ctx.ReqConfigProfile = autoConfigProfile
			ctx.Message = "prompt"
			ctx.ReqBody = tt.body
			ctx.ReqHeaders = map[string]string{}

			applyConfigProfile(ctx, pods)

			require.NotNil(t, ctx.ConfigProfile)
			assert.Equal(t, tt.wantName, ctx.ReqConfigProfile)
			assert.Equal(t, tt.wantName, ctx.RespHeaders[HeaderAIBrixConfigProfile])
		})
	}
}

func TestDeriveRoutingStrategyFromContext(t *testing.T) {
	// lockedRoutingStrategy (model-wide) always wins over header and profile.
	lockedCtx := &types.RoutingContext{
		ConfigProfile: &types.ResolvedConfigProfile{
			LockedRoutingStrategy: "pd",
			RoutingStrategy:       "random",
		},
		ReqHeaders: map[string]string{HeaderRoutingStrategy: "throughput"},
	}

	// Header wins over profile strategy.
	headerCtx := &types.RoutingContext{
		ConfigProfile: &types.ResolvedConfigProfile{RoutingStrategy: "random"},
		ReqHeaders:    map[string]string{HeaderRoutingStrategy: "throughput"},
	}

	// Profile strategy is used when no header is present.
	profileCtx := &types.RoutingContext{
		ConfigProfile: &types.ResolvedConfigProfile{RoutingStrategy: "least-request"},
		ReqHeaders:    map[string]string{},
	}

	// Case-insensitive header key match.
	headerCaseCtx := &types.RoutingContext{
		ConfigProfile: &types.ResolvedConfigProfile{RoutingStrategy: "random"},
		ReqHeaders:    map[string]string{"Routing-Strategy": "pd"},
	}

	// Nothing set: falls back to environment default.
	emptyCtx := &types.RoutingContext{ReqHeaders: map[string]string{}}

	tests := []struct {
		name   string
		ctx    *types.RoutingContext
		want   string
		wantOK bool
	}{
		{"locked strategy wins over header and profile", lockedCtx, "pd", true},
		{"header wins over profile strategy", headerCtx, "throughput", true},
		{"profile strategy used when header absent", profileCtx, "least-request", true},
		{"case-insensitive header key", headerCaseCtx, "pd", true},
		{"falls back to env when nothing set", emptyCtx, defaultRoutingStrategy, defaultRoutingStrategyEnabled},
		{"nil context falls back to env", nil, defaultRoutingStrategy, defaultRoutingStrategyEnabled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := deriveRoutingStrategyFromContext(tt.ctx)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}
