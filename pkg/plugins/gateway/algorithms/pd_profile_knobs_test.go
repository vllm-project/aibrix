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

package routingalgorithms

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/selector"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file covers the PD halves of the profile knob flow that are specific to
// the PD path: the prompt-length bucketing switch and the parking of the
// resolved PD overrides on the request's leg, where the async prefill and
// decode-abort paths read them after the pooled routing context is recycled.
// The resolution rules themselves are covered in routing_profile_knobs_test.go.

// pdKnobsContext builds a PD request the way the gateway does: a routing
// context that has a PD leg, carrying the profile under test. These tests read
// the overrides off that leg, where the PD router parks them.
func pdKnobsContext(t *testing.T, routingConfig string) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterPD, "model", "message", "req-pd-knobs", "user")
	t.Cleanup(ctx.Delete)
	if routingConfig != "" {
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RoutingConfig: json.RawMessage(routingConfig),
			Routing:       configprofiles.ParseRoutingConfig(json.RawMessage(routingConfig)),
		}
	}
	return ctx
}

// resolvedPDOverrides is the parking step of pdRouter.Route: the request's
// resolved PD overrides, which the PD read sites then read off the request's
// leg rather than the pooled routing context.
func resolvedPDOverrides(ctx *types.RoutingContext) *types.PDOverrides {
	pdOverrides := ctx.RoutingOverrides().PD
	return &pdOverrides
}

func TestEffectivePromptLengthBucketing(t *testing.T) {
	withPromptLengthBucketing(t, false)
	assert.False(t, effectivePromptLengthBucketing(pdKnobsContext(t, "")),
		"without a profile the process default decides")

	profiled := pdKnobsContext(t, `{"promptLengthBucketing":true}`)
	ResolveRoutingOverrides(profiled)
	profiled.SetPDOverrides(resolvedPDOverrides(profiled))
	assert.True(t, effectivePromptLengthBucketing(profiled),
		"the profile turns bucketing on for its requests only")

	withPromptLengthBucketing(t, true)
	off := pdKnobsContext(t, `{"promptLengthBucketing":false}`)
	ResolveRoutingOverrides(off)
	off.SetPDOverrides(resolvedPDOverrides(off))
	assert.False(t, effectivePromptLengthBucketing(off), "and off again")
}

func TestRouteParksProfileKnobsOnTheLeg(t *testing.T) {
	withDefaultOverrides(t, probeRoutingDefaults())

	decodePod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "decode-1"},
		Status:     v1.PodStatus{PodIP: "127.0.0.2"},
	}
	newRouter := func() *pdRouter {
		r := &pdRouter{pendingDecodeTracker: pd.NewPendingDecodeTracker()}
		r.podSelector = selector.NewDefaultSelector(func(_ *types.RoutingContext, _ []*v1.Pod) (*v1.Pod, *v1.Pod, error) {
			return nil, decodePod, nil
		})
		return r
	}

	ctx := pdKnobsContext(t, `{"pd":{"decodeAbortTimeout":0}}`)
	ctx.Engine = VLLMEngine
	ctx.ReqPath = "/v1/chat/completions"
	ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	ResolveRoutingOverrides(ctx)

	_, err := newRouter().Route(ctx, &utils.PodArray{Pods: []*v1.Pod{}})
	require.NoError(t, err)

	assert.Equal(t, time.Duration(0), ctx.PDOverrides().Abort.Timeout,
		"Route must park the resolved profile overrides on the request's PD leg")

	plain := pdKnobsContext(t, "")
	plain.Engine = VLLMEngine
	plain.ReqPath = "/v1/chat/completions"
	plain.ReqBody = []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	_, err = newRouter().Route(plain, &utils.PodArray{Pods: []*v1.Pod{}})
	require.NoError(t, err)
	assert.Equal(t, probeRoutingDefaults().PD.Abort.Timeout, plain.PDOverrides().Abort.Timeout,
		"a request without profile knobs keeps the process defaults")
}
