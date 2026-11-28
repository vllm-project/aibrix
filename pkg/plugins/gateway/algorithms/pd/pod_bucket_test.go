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

package pd

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
)

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

func TestRoutablePDLabelsAndEligibility(t *testing.T) {
	rs, role, ok := RoutablePDLabels(nil)
	assert.False(t, ok)
	assert.Empty(t, rs)
	assert.Empty(t, role)
	assert.False(t, IsPodWithHTTPServer(nil))

	missingRole := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p0", Labels: map[string]string{RoleSetLabel: "rs1"}}}
	_, _, ok = RoutablePDLabels(missingRole)
	assert.False(t, ok)

	nonHTTPWorker := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "p-rank1",
		Labels: map[string]string{
			RoleSetLabel:       "rs1",
			RoleLabel:          RolePrefill,
			PodGroupIndexLabel: "1",
		},
	}}
	_, _, ok = RoutablePDLabels(nonHTTPWorker)
	assert.False(t, ok)

	headPod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "p-rank0",
		Labels: map[string]string{
			RoleSetLabel:       "rs1",
			RoleLabel:          RoleDecode,
			PodGroupIndexLabel: "0",
		},
	}}
	rs, role, ok = RoutablePDLabels(headPod)
	assert.True(t, ok)
	assert.Equal(t, "rs1", rs)
	assert.Equal(t, RoleDecode, role)

	assert.False(t, IsRolesetEligible(0, 0))
	assert.False(t, IsRolesetEligible(1, 0))
	assert.False(t, IsRolesetEligible(0, 1))
	assert.True(t, IsRolesetEligible(1, 1))
}

func TestPromptLenBucketBoundsAndSuitability(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *types.RoutingConfig
		wantMin   int
		wantMax   int
		checkLens map[int]bool
	}{
		{
			name:      "nil config defaults to unbounded",
			cfg:       nil,
			wantMin:   0,
			wantMax:   math.MaxInt32,
			checkLens: map[int]bool{0: true, 100: true, 999999: true},
		},
		{
			name:      "empty config defaults to unbounded",
			cfg:       &types.RoutingConfig{},
			wantMin:   0,
			wantMax:   math.MaxInt32,
			checkLens: map[int]bool{0: true, 500: true},
		},
		{
			name:      "bounded inclusive range",
			cfg:       &types.RoutingConfig{PromptLenBucketMinLength: intPtr(10), PromptLenBucketMaxLength: intPtr(50)},
			wantMin:   10,
			wantMax:   50,
			checkLens: map[int]bool{9: false, 10: true, 30: true, 50: true, 51: false},
		},
		{
			name:      "non-positive bounds fall back to defaults",
			cfg:       &types.RoutingConfig{PromptLenBucketMinLength: intPtr(-5), PromptLenBucketMaxLength: intPtr(0)},
			wantMin:   0,
			wantMax:   math.MaxInt32,
			checkLens: map[int]bool{0: true, 12345: true},
		},
		{
			name:      "inverted range matches nothing",
			cfg:       &types.RoutingConfig{PromptLenBucketMinLength: intPtr(100), PromptLenBucketMaxLength: intPtr(10)},
			wantMin:   100,
			wantMax:   10,
			checkLens: map[int]bool{0: false, 10: false, 50: false, 100: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMin, gotMax := PromptLenBucketBounds(tt.cfg)
			assert.Equal(t, tt.wantMin, gotMin)
			assert.Equal(t, tt.wantMax, gotMax)
			for length, want := range tt.checkLens {
				assert.Equal(t, want, IsPromptLengthInBucket(tt.cfg, length), "promptLength=%d", length)
			}
		})
	}
}

func TestIsCombinedRoutingConfig(t *testing.T) {
	assert.False(t, IsCombinedRoutingConfig(nil))
	assert.False(t, IsCombinedRoutingConfig(&types.RoutingConfig{}))
	assert.False(t, IsCombinedRoutingConfig(&types.RoutingConfig{Combined: boolPtr(false)}))
	assert.True(t, IsCombinedRoutingConfig(&types.RoutingConfig{Combined: boolPtr(true)}))
}

func TestResolvePodRoutingConfig_CachingAndModelConfigPrecedence(t *testing.T) {
	assert.Nil(t, ResolvePodRoutingConfig(nil, nil, nil))

	ctx := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req", "user")
	annoShort := `{"defaultProfile":"pd","profiles":{"pd":{"routingStrategy":"pd","routingConfig":{"promptLenBucketMinLength":1,"promptLenBucketMaxLength":100,"combined":false}}}}`
	annoCombined := `{"defaultProfile":"pd","profiles":{"pd":{"routingStrategy":"pd","routingConfig":{"promptLenBucketMinLength":0,"promptLenBucketMaxLength":0,"combined":true}}}}`

	podA := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p-a", Annotations: map[string]string{constants.ModelAnnoConfig: annoShort}}}
	podB := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p-b", Annotations: map[string]string{constants.ModelAnnoConfig: annoShort}}}
	podCombined := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p-c", Annotations: map[string]string{constants.ModelAnnoConfig: annoCombined}}}
	podPlain := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p-plain"}}

	var cfgCache map[string]*types.RoutingConfig
	cfgA := ResolvePodRoutingConfig(ctx, podA, &cfgCache)
	cfgB := ResolvePodRoutingConfig(ctx, podB, &cfgCache)
	require.NotNil(t, cfgA)
	assert.Same(t, cfgA, cfgB, "identical annotation should reuse cached RoutingConfig pointer")
	assert.Len(t, cfgCache, 1)

	cfgC := ResolvePodRoutingConfig(ctx, podCombined, &cfgCache)
	require.NotNil(t, cfgC)
	assert.True(t, IsCombinedRoutingConfig(cfgC))
	assert.True(t, IsCombinedPod(ctx, podCombined))
	assert.False(t, IsCombinedPod(ctx, podA))
	assert.False(t, IsCombinedPod(ctx, podPlain))

	assert.Nil(t, ResolvePodRoutingConfig(ctx, podPlain, &cfgCache))
	minLen, maxLen := PodPromptLenBucketBounds(ctx, podA)
	assert.Equal(t, 1, minLen)
	assert.Equal(t, 100, maxLen)
	assert.True(t, IsPodSuitableForPromptLength(ctx, podA, 50))
	assert.False(t, IsPodSuitableForPromptLength(ctx, podA, 101))
	assert.True(t, IsPodSuitableForPromptLength(ctx, podPlain, 99999))

	// Model-level config takes precedence over pod annotation.
	ctxWithModelCfg := types.NewRoutingContext(context.Background(), "pd", "model", "msg", "req-2", "user")
	ctxWithModelCfg.ModelConfig = `{"defaultProfile":"pd","profiles":{"pd":{"routingStrategy":"pd","routingConfig":{"promptLenBucketMinLength":500,"promptLenBucketMaxLength":600,"combined":true}}}}`
	assert.False(t, IsPodSuitableForPromptLength(ctxWithModelCfg, podA, 50))
	assert.True(t, IsPodSuitableForPromptLength(ctxWithModelCfg, podA, 550))
	assert.True(t, IsCombinedPod(ctxWithModelCfg, podA))

	// RoutingConfigCache zero value memoizes within one pass and handles nil receiver.
	var passCache RoutingConfigCache
	c1 := passCache.Resolve(ctx, podA)
	c2 := passCache.Resolve(ctx, podB)
	require.NotNil(t, c1)
	assert.Same(t, c1, c2)
	var nilCache *RoutingConfigCache
	assert.NotNil(t, nilCache.Resolve(ctx, podA))
}
