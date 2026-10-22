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

package gateway

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/vtc"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// With one ready pod the gateway skips Route and runs only the router's
// post-route hook, so that hook must charge the vtc-basic user.
func TestSelectTargetPodSingleCandidateChargesVTCUser(t *testing.T) {
	cache.InitForTest()
	config := vtc.DefaultVTCConfig()
	tracker := vtc.NewInMemorySlidingWindowTokenTracker(&config)
	vtcRouter, err := vtc.NewBasicVTCRouter(tracker, vtc.NewSimpleTokenEstimator(), &config)
	require.NoError(t, err)

	manager := routing.NewRouterManager()
	manager.RegisterProvider(vtc.RouterVTCBasic, func(*types.RoutingContext) (types.Router, error) {
		return vtcRouter, nil
	})
	server := &Server{cache: &MockCache{}, routerManager: manager}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-a"},
		Status: v1.PodStatus{
			PodIP:      "10.0.0.1",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}

	routeCtx := types.NewRoutingContext(context.Background(), vtc.RouterVTCBasic, "model", "please summarize this document", "req", "bob")
	defer routeCtx.Delete()
	_, err = server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: []*v1.Pod{pod}}, "")
	require.NoError(t, err)

	tokens, err := tracker.GetTokenCount(context.Background(), "bob")
	require.NoError(t, err)
	require.Greater(t, tokens, 0.0, "the single-candidate fast path charges the user")
}
