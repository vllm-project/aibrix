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
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"

	v1 "k8s.io/api/core/v1"
)

// inflightProfile returns the resolved config profile a capped request carries: the per-replica
// cap, plus the candidate stage when k is positive. requestsInflight has no environment-variable
// form and lives on the model profile, so the tests set the resolved fields directly, the same
// way applyConfigProfile does.
func inflightProfile(inflight int64, k int) *types.ResolvedConfigProfile {
	cp := &types.ResolvedConfigProfile{RequestsInflight: inflight}
	if k > 0 {
		raw := json.RawMessage(fmt.Sprintf(`{"candidate":{"k":%d}}`, k))
		cp.RoutingConfig = raw
		cp.Routing = configprofiles.ParseRoutingConfig(raw)
	}
	return cp
}

// TestSelectTargetPod_AllCandidatesSaturated pins the rejection rule for this change: the cap is
// checked on the candidate set, and when every candidate is at the cap the request gets the
// existing 429 rather than a fallback read of the full ready set.
//
// The read size is the assertion that matters. Before this change the filter ran over all ready
// pods, so a saturated fleet issued one read of every pod per decision. Here the filter must see
// only the K candidates, and the router must never run.
func TestSelectTargetPod_AllCandidatesSaturated(t *testing.T) {
	pods := []*v1.Pod{
		podWithReplicaInflight("a", 1),
		podWithReplicaInflight("b", 1),
		podWithReplicaInflight("c", 1),
		podWithReplicaInflight("d", 1),
	}
	saturated := map[string]int64{"ns/a": 1, "ns/b": 1, "ns/c": 1, "ns/d": 1}

	var readSizes []int
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.MatchedBy(func(read []*v1.Pod) bool {
		readSizes = append(readSizes, len(read))
		return true
	})).Return(saturated, nil)

	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterLeastRequest, func(*types.RoutingContext) (types.Router, error) {
		return routeFunc(func(*types.RoutingContext, types.PodList) (string, error) {
			t.Error("the router must not run when every candidate is at the cap")
			return "", errors.New("unexpected route call")
		}), nil
	})
	s := &Server{cache: mockCache, routerManager: manager}

	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "hello", "r1", "")
	t.Cleanup(routeCtx.Delete)
	routeCtx.ConfigProfile = inflightProfile(1, 2)

	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.True(t, errors.Is(err, errReplicaInflightExceeded),
		"every candidate is at the cap, so the request is rejected with the existing overload error")
	require.Len(t, readSizes, 1, "the decision reads the running counts once")
	assert.LessOrEqual(t, readSizes[0], 2,
		"the filter reads the candidate set, not the ready set: got %d pods of %d", readSizes[0], len(pods))
}

// TestSelectTargetPod_PartialSaturationKeepsAdmissiblePods covers the ordinary case: one pod is at
// the cap, the rest are not, and only the pods with room reach the router.
func TestSelectTargetPod_PartialSaturationKeepsAdmissiblePods(t *testing.T) {
	pods := []*v1.Pod{
		podWithReplicaInflight("a", 1),
		podWithReplicaInflight("b", 1),
		podWithReplicaInflight("c", 1),
	}
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.Anything).
		Return(map[string]int64{"ns/a": 1, "ns/b": 0, "ns/c": 0}, nil)

	var routed int
	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterLeastRequest, func(*types.RoutingContext) (types.Router, error) {
		return routeFunc(func(ctx *types.RoutingContext, list types.PodList) (string, error) {
			routed = list.Len()
			ctx.SetTargetPod(list.All()[0])
			return ctx.TargetAddress(), nil
		}), nil
	})
	s := &Server{cache: mockCache, routerManager: manager}

	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "hello", "r2", "")
	t.Cleanup(routeCtx.Delete)
	routeCtx.ConfigProfile = inflightProfile(1, 0)

	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.NoError(t, err)
	assert.Equal(t, 2, routed, "the pod at the cap is dropped before the router sees the set")
	assert.NotEqual(t, "a", routeCtx.TargetPod().Name, "the saturated pod is not chosen")
}

// TestSelectTargetPod_PDSaturatedRejectsBeforeEngineValidation is the regression test for the
// ordering this change had to preserve in the exclusive branch. PD validates that every pod
// speaks the same engine, and that validation used to run before the cap filter. With the filter
// first, a request that is about to be rejected for saturation is not failed on a pod it never
// uses, so the error is the overload error and not an engine mismatch.
func TestSelectTargetPod_PDSaturatedRejectsBeforeEngineValidation(t *testing.T) {
	podA := podWithReplicaInflight("a", 1)
	podB := podWithReplicaInflight("b", 1)
	podB.Labels = map[string]string{constants.ModelLabelEngine: "sglang"}
	pods := []*v1.Pod{podA, podB}

	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.Anything).
		Return(map[string]int64{"ns/a": 1, "ns/b": 1}, nil)

	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterPD, func(*types.RoutingContext) (types.Router, error) {
		return routeFunc(func(*types.RoutingContext, types.PodList) (string, error) {
			t.Error("the router must not run when every replica is at the cap")
			return "", errors.New("unexpected route call")
		}), nil
	})
	s := &Server{cache: mockCache, routerManager: manager}

	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, "m", "hello", "r3", "")
	t.Cleanup(routeCtx.Delete)
	routeCtx.ConfigProfile = inflightProfile(1, 0)

	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.True(t, errors.Is(err, errReplicaInflightExceeded),
		"the cap is checked before engine validation, so mixed engines do not mask the 429: %v", err)
	assert.NotContains(t, err.Error(), "inconsistent LLM engines")
}

// TestSelectTargetPod_CandidateReadErrorFailsOpen pins the failure mode of the bounded read: a
// Redis error must not turn into a rejection. The filter keeps the candidate set it was given and
// routing continues, which is what the full-fleet filter already did.
func TestSelectTargetPod_CandidateReadErrorFailsOpen(t *testing.T) {
	pods := []*v1.Pod{
		podWithReplicaInflight("a", 1),
		podWithReplicaInflight("b", 1),
		podWithReplicaInflight("c", 1),
	}
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.Anything).
		Return(nil, errors.New("redis: connection refused"))

	var routed int
	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterLeastRequest, func(*types.RoutingContext) (types.Router, error) {
		return routeFunc(func(ctx *types.RoutingContext, list types.PodList) (string, error) {
			routed = list.Len()
			ctx.SetTargetPod(list.All()[0])
			return ctx.TargetAddress(), nil
		}), nil
	})
	s := &Server{cache: mockCache, routerManager: manager}

	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "hello", "r4", "")
	t.Cleanup(routeCtx.Delete)
	routeCtx.ConfigProfile = inflightProfile(1, 2)

	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.NoError(t, err, "a failed running-request read fails open, it does not reject the request")
	assert.Equal(t, 2, routed, "the candidate set reaches the router unchanged")
}
