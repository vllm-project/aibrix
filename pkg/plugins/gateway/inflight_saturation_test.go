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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

	before := inflightRejectionCount(t, "m", inflightScopeCandidates)
	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.True(t, errors.Is(err, errReplicaInflightExceeded),
		"every candidate is at the cap, so the request is rejected with the existing overload error")
	require.NotNil(t, inflightRejections, "the rejection counter is registered on first use")
	assert.Equal(t, before+1, inflightRejectionCount(t, "m", inflightScopeCandidates),
		"the rejection is counted against the candidate set")
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

	before := inflightRejectionCount(t, "m", inflightScopeFleet)
	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.True(t, errors.Is(err, errReplicaInflightExceeded),
		"the cap is checked before engine validation, so mixed engines do not mask the 429: %v", err)
	require.NotNil(t, inflightRejections, "the rejection counter is registered on first use")
	assert.Equal(t, before+1, inflightRejectionCount(t, "m", inflightScopeFleet),
		"an exclusive strategy checks the cap on the ready set, so the rejection is fleet scoped")
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

// TestSelectTargetPod_PartialSaturationOnCandidateSet covers the narrowing path rather than the
// plain filter: with K=2 the stage keeps one ranked pod and one random pod. The pod at the cap is
// also the best estimated pod, so it takes the ranked slot and must be dropped by the filter,
// leaving exactly one admissible pod for the router.
func TestSelectTargetPod_PartialSaturationOnCandidateSet(t *testing.T) {
	pods := []*v1.Pod{
		podWithReplicaInflight("a", 1),
		podWithReplicaInflight("b", 1),
		podWithReplicaInflight("c", 1),
		podWithReplicaInflight("d", 1),
	}
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.Anything).
		Return(map[string]int64{"ns/a": 1, "ns/b": 0, "ns/c": 0, "ns/d": 0}, nil)

	var routed []string
	estimates := estimateRouter{scores: map[string]float64{"a": 100, "b": 1, "c": 0, "d": 0},
		route: func(ctx *types.RoutingContext, list types.PodList) (string, error) {
			for _, pod := range list.All() {
				routed = append(routed, pod.Name)
			}
			ctx.SetTargetPod(list.All()[0])
			return ctx.TargetAddress(), nil
		}}
	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterLeastRequest, func(*types.RoutingContext) (types.Router, error) {
		return estimates, nil
	})
	s := &Server{cache: mockCache, routerManager: manager}

	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "hello", "r5", "")
	t.Cleanup(routeCtx.Delete)
	routeCtx.ConfigProfile = inflightProfile(1, 2)

	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.NoError(t, err)
	require.Len(t, routed, 1, "the ranked slot was the pod at the cap, so one pod is left")
	assert.NotEqual(t, "a", routed[0], "a saturated pod never reaches the router")
	assert.NotEqual(t, "a", routeCtx.TargetPod().Name)
}

// TestSelectTargetPod_StaleApproxCountsDoNotReject covers the false-429 path the pre-exclusion
// must not create. The candidate stage's in-memory counts are a hint, not the decision: when they
// say every candidate is at the cap but the Redis counts still show room, the request must route,
// because the filter is the source of truth. This pins the soft fallback across the stage
// boundary.
func TestSelectTargetPod_StaleApproxCountsDoNotReject(t *testing.T) {
	pods := []*v1.Pod{
		podWithReplicaInflight("a", 1),
		podWithReplicaInflight("b", 1),
		podWithReplicaInflight("c", 1),
		podWithReplicaInflight("d", 1),
	}
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequestsApprox", mock.Anything).
		Return(map[string]int64{"ns/a": 1, "ns/b": 1, "ns/c": 1, "ns/d": 1})
	mockCache.On("GetPodsRunningRequests", mock.Anything).
		Return(map[string]int64{"ns/a": 0, "ns/b": 0, "ns/c": 0, "ns/d": 0}, nil)

	var routed []string
	estimates := estimateRouter{scores: map[string]float64{"a": 100, "b": 1, "c": 0, "d": 0},
		route: func(ctx *types.RoutingContext, list types.PodList) (string, error) {
			for _, pod := range list.All() {
				routed = append(routed, pod.Name)
				if pod.Name == "a" {
					ctx.SetTargetPod(pod)
				}
			}
			return ctx.TargetAddress(), nil
		}}
	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterLeastRequest, func(*types.RoutingContext) (types.Router, error) {
		return estimates, nil
	})
	s := &Server{cache: mockCache, routerManager: manager}

	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "hello", "r6", "")
	t.Cleanup(routeCtx.Delete)
	routeCtx.ConfigProfile = inflightProfile(1, 2)

	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.NoError(t, err, "stale in-memory counts must not turn room in Redis into a 429")
	require.Contains(t, routed, "a", "the best estimate survives the filter")
	assert.Equal(t, "a", routeCtx.TargetPod().Name, "the best estimate still routes")
	mockCache.AssertCalled(t, "GetPodsRunningRequestsApprox", mock.Anything)
}

// TestSelectTargetPod_GateRunsAfterFilter pins the order the gate depends on. The saturated pod
// also carries the highest count, so a gate placed before the filter would see a spread of 1 over
// the ready set, record a load-imbalance event, and trim the set. After the filter it sees two
// idle pods, which is not imbalanced, and the counter stays put. The profile lowers
// imbalanceFactor so a spread of 1 counts as imbalanced for the three-pod comparison.
func TestSelectTargetPod_GateRunsAfterFilter(t *testing.T) {
	const model = "gate-order-test"
	pods := []*v1.Pod{
		podWithReplicaInflight("a", 1),
		podWithReplicaInflight("b", 1),
		podWithReplicaInflight("c", 1),
	}
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.Anything).
		Return(map[string]int64{"ns/a": 1, "ns/b": 0, "ns/c": 0}, nil)

	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterLeastRequest, func(*types.RoutingContext) (types.Router, error) {
		return routeFunc(func(ctx *types.RoutingContext, list types.PodList) (string, error) {
			ctx.SetTargetPod(list.All()[0])
			return ctx.TargetAddress(), nil
		}), nil
	})
	s := &Server{cache: mockCache, routerManager: manager}

	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, model, "hello", "r6", "")
	t.Cleanup(routeCtx.Delete)
	profile := inflightProfile(1, 0)
	raw := json.RawMessage(`{"loadBalance":{"imbalanceFactor":0.5,"imbalanceMinGap":1}}`)
	profile.RoutingConfig = raw
	profile.Routing = configprofiles.ParseRoutingConfig(raw)
	routeCtx.ConfigProfile = profile

	before := loadImbalanceCount(t, model)
	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.NoError(t, err)
	assert.Equal(t, before, loadImbalanceCount(t, model),
		"the gate compares only the pods that can still admit, so the saturated pod cannot trigger it")
}

// inflightRejectionCount reads the rejection counter for one model and scope. The counter is
// created on first use, and the test that calls this has already caused a rejection.
func inflightRejectionCount(t *testing.T, model, scope string) float64 {
	t.Helper()
	if inflightRejections == nil {
		return 0
	}
	return testutil.ToFloat64(inflightRejections.WithLabelValues(model, scope))
}

// loadImbalanceCount reads aibrix_load_imbalance_total for one model out of the default
// registry, and reports 0 when the metric has never been recorded.
func loadImbalanceCount(t *testing.T, model string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "aibrix_load_imbalance_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "model" && label.GetValue() == model {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// estimateRouter routes to the first pod it is handed and reports a fixed estimate per pod name,
// so the candidate stage's ranked slot is deterministic.
type estimateRouter struct {
	scores map[string]float64
	route  func(ctx *types.RoutingContext, pods types.PodList) (string, error)
}

func (r estimateRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	return r.route(ctx, pods)
}

func (r estimateRouter) ScoreAll(_ *types.RoutingContext, pods types.PodList) ([]float64, []bool, error) {
	scores := make([]float64, pods.Len())
	scored := make([]bool, pods.Len())
	for i, pod := range pods.All() {
		scores[i] = r.scores[pod.Name]
		scored[i] = true
	}
	return scores, scored, nil
}

func (r estimateRouter) CandidateScores(ctx *types.RoutingContext, pods types.PodList) ([]float64, []bool, error) {
	return r.ScoreAll(ctx, pods)
}

func (r estimateRouter) Polarity() types.Polarity { return types.PolarityMost }

// The single-candidate fast path would skip Route for a one-pod set, and this test needs the
// router to see the filtered set.
func (r estimateRouter) BypassSingleCandidate() bool { return false }

// TestSelectTargetPod_RejectionSpanAttributes pins the two attributes the rejection writes. They
// are the per-request detail behind the scope counter: with a trace in hand, the cap and the
// number of pods the filter considered say whether the sampled set or the fleet was exhausted.
func TestSelectTargetPod_RejectionSpanAttributes(t *testing.T) {
	pods := []*v1.Pod{
		podWithReplicaInflight("a", 1),
		podWithReplicaInflight("b", 1),
		podWithReplicaInflight("c", 1),
		podWithReplicaInflight("d", 1),
	}
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.Anything).
		Return(map[string]int64{"ns/a": 1, "ns/b": 1, "ns/c": 1, "ns/d": 1}, nil)

	manager := routing.NewRouterManager()
	manager.RegisterProvider(routing.RouterLeastRequest, func(*types.RoutingContext) (types.Router, error) {
		return routeFunc(func(*types.RoutingContext, types.PodList) (string, error) {
			return "", errors.New("unexpected route call")
		}), nil
	})
	s := &Server{cache: mockCache, routerManager: manager}

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	_, span := provider.Tracer("test").Start(context.Background(), "rejection")

	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "hello", "r7", "")
	t.Cleanup(routeCtx.Delete)
	routeCtx.ConfigProfile = inflightProfile(1, 2)
	routeCtx.Span = span

	_, err := s.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.True(t, errors.Is(err, errReplicaInflightExceeded))
	span.End()

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	attrs := make(map[string]int64, len(ended[0].Attributes()))
	for _, kv := range ended[0].Attributes() {
		if kv.Value.Type() == attribute.INT64 {
			attrs[string(kv.Key)] = kv.Value.AsInt64()
		}
	}
	assert.Equal(t, int64(1), attrs["inflight_cap"])
	assert.Equal(t, int64(2), attrs["inflight_considered_pods"],
		"the attribute records the candidate set the filter read, not the ready set")
}
