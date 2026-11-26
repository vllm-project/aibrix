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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_ValidateRoutingStrategy(t *testing.T) {
	var tests = []struct {
		routingStrategy    string
		message            string
		expectedValidation bool
	}{
		{
			routingStrategy:    "",
			message:            "empty routing strategy",
			expectedValidation: false,
		},
		{
			routingStrategy:    "  ",
			message:            "spaced routing strategy",
			expectedValidation: false,
		},
		{
			routingStrategy:    "random",
			message:            "random routing strategy",
			expectedValidation: true,
		},
		{
			routingStrategy:    "least-request",
			message:            "least-request routing strategy",
			expectedValidation: true,
		},
		{
			routingStrategy:    "rrandom",
			message:            "misspell routing strategy",
			expectedValidation: false,
		},
	}
	cache.InitForTest()
	routing.Init()
	for _, tt := range tests {
		_, currentValidation := routing.Validate(tt.routingStrategy)
		assert.Equal(t, tt.expectedValidation, currentValidation, tt.message)
	}
}

// Test_selectTargetPod tests the selectTargetPod method for various pod selection scenarios
func Test_selectTargetPod(t *testing.T) {
	// Initialize routing algorithms for the test
	routing.Init()

	// Define test cases for different pod selection and error scenarios
	tests := []struct {
		name           string
		pods           types.PodList
		mockSetup      func(*mockRouter, types.RoutingAlgorithm)
		expectedError  bool
		expectedPodIP  string
		externalFilter string
	}{
		{
			name: "routing.Route returns error",
			pods: &utils.PodArray{Pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			},
				{
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				}}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that returns an error
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("", errors.New("test error"))

			},
			expectedError: true,
		},
		{
			name: "no pods available",
			pods: &utils.PodArray{Pods: []*v1.Pod{}},
			mockSetup: func(m *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router, but no pods are available
				routing.Register(algo, func() (types.Router, error) {
					return m, nil
				})
				// No expectations needed as pods.Len() == 0
			},
			expectedError: true,
		},
		{
			name: "no ready pods available",
			pods: &utils.PodArray{Pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}},
				},
			}}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router, but no pods are ready
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				// No expectations needed as no ready pods
			},
			expectedError: true,
		},
		{
			name: "single ready pod",
			pods: &utils.PodArray{Pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "1.2.3.4",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			}}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router, but only one pod is ready so Route should not be called
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				// Explicitly set expectation that Route should not be called
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError: false,
			expectedPodIP: "1.2.3.4:8000",
		},
		{
			name: "single ready pod out of two",
			pods: &utils.PodArray{Pods: []*v1.Pod{{
				Status: v1.PodStatus{
					PodIP:      "8.9.10.11",
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			},
				{
					Status: v1.PodStatus{
						PodIP:      "4.5.6.7",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}},
					},
				}}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router, but only one pod is ready so Route should not be called
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				// Explicitly set expectation that Route should not be called
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError: false,
			expectedPodIP: "8.9.10.11:8000",
		},
		{
			name: "multiple ready pods",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			},
			expectedError: false,
			expectedPodIP: "1.2.3.4:8000",
		},
		{
			name: "single external filter",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "sad",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "1.2.3.4:8000",
			externalFilter: "foo=bar",
		},
		{
			name: "slice external filter",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"env": "prod",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
							"env": "prod",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "2.3.4.5",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "2.3.4.5:8000",
			externalFilter: "foo=bar,env=prod",
		},
		{
			name: "external filter and route multiple pods",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			},
			expectedError:  false,
			expectedPodIP:  "1.2.3.4:8000",
			externalFilter: "foo=bar",
		},
		{
			name: "external filter use 'in' and route multiple pods",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bug",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			},
			expectedError:  false,
			expectedPodIP:  "1.2.3.4:8000",
			externalFilter: "foo in (bar, bug)",
		},
		{
			name: "external filter use 'not in' and route multiple pods",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bug",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "par",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "2.3.4.5",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "2.3.4.5:8000",
			externalFilter: "foo notin (bar, bug)",
		},
		{
			name: "external filter use !=",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bug",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "5.6.7.8:8000",
			externalFilter: "foo!=bar",
		},
		{
			name: "external filter with key exists",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "sad",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).Once()
			},
			expectedError:  false,
			expectedPodIP:  "1.2.3.4:8000",
			externalFilter: "foo",
		},
		{
			name: "external filter with key not exists",
			pods: &utils.PodArray{Pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "1.2.3.4",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"sad": "sad",
						},
					},
					Status: v1.PodStatus{
						PodIP:      "5.6.7.8",
						Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
					},
				},
			}},
			mockSetup: func(mockRouter *mockRouter, algo types.RoutingAlgorithm) {
				// Register a mock router that selects a pod from multiple ready pods
				routing.Register(algo, func() (types.Router, error) {
					return mockRouter, nil
				})
				mockRouter.On("Route", mock.Anything, mock.Anything).Unset()
			},
			expectedError:  false,
			expectedPodIP:  "5.6.7.8:8000",
			externalFilter: "!foo",
		},
	}

	for _, tt := range tests {
		// Run each test case as a subtest
		t.Run(tt.name, func(subtest *testing.T) {
			subtest.Parallel() // Run subtests in parallel
			mockRouter := new(mockRouter)
			routingAlgo := types.RoutingAlgorithm(fmt.Sprintf("test-router-%s", tt.name))

			// Set up the mock router and register the routing algorithm for this test
			tt.mockSetup(mockRouter, routingAlgo)
			routing.Init()

			server := &Server{}
			routeCtx := types.NewRoutingContext(context.Background(), routingAlgo, "test-model", "test-message", "test-request", "test-user")

			// Call selectTargetPod and check the result
			podIP, err := server.selectTargetPod(context.Background(), routeCtx, tt.pods, tt.externalFilter)

			if tt.expectedError {
				assert.Error(subtest, err)
			} else {
				assert.NoError(subtest, err)
				assert.Equal(subtest, tt.expectedPodIP, podIP)
			}

			// Ensure all mock expectations are met
			mockRouter.AssertExpectations(subtest)
		})
	}
}

func Test_selectTargetPod_PDEngineValidation(t *testing.T) {
	ready := func(name, ip, engine string) *v1.Pod {
		labels := map[string]string{}
		if engine != "" {
			labels[constants.ModelLabelEngine] = engine
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Status: v1.PodStatus{
				PodIP:      ip,
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}

	t.Run("rejects mismatched engines before Route", func(t *testing.T) {
		mockRouter := new(mockRouter)
		routing.Register(routing.RouterPD, func() (types.Router, error) {
			return mockRouter, nil
		})
		routing.Init()

		server := &Server{}
		routeCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, "test-model", "msg", "req-engine", "user")
		pods := &utils.PodArray{Pods: []*v1.Pod{
			ready("prefill-1", "10.0.0.1", "vllm"),
			ready("decode-1", "10.0.0.2", "sglang"),
		}}

		_, err := server.selectTargetPod(context.Background(), routeCtx, pods, "")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "engine validation failed")
		assert.Contains(t, err.Error(), "inconsistent LLM engines")
		mockRouter.AssertNotCalled(t, "Route", mock.Anything, mock.Anything)
	})

	t.Run("sets engine on routing context when consistent", func(t *testing.T) {
		mockRouter := new(mockRouter)
		routing.Register(routing.RouterPD, func() (types.Router, error) {
			return mockRouter, nil
		})
		routing.Init()
		mockRouter.On("Route", mock.Anything, mock.Anything).Return("10.0.0.2:8000", nil)

		server := &Server{}
		routeCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, "test-model", "msg", "req-engine-ok", "user")
		pods := &utils.PodArray{Pods: []*v1.Pod{
			ready("prefill-1", "10.0.0.1", "vllm"),
			ready("decode-1", "10.0.0.2", "vllm"),
		}}

		addr, err := server.selectTargetPod(context.Background(), routeCtx, pods, "")
		assert.NoError(t, err)
		assert.Equal(t, "10.0.0.2:8000", addr)
		assert.Equal(t, "vllm", routeCtx.Engine)
		mockRouter.AssertExpectations(t)
	})
}

// Test_selectTargetPod_SessionAffinitySurvivesSingleReadyPodFastPath is a regression test for
// the single-ready-pod fast path in selectTargetPod (readyPods == 1, single port, non-exclusive
// strategy): that path used to return the pod directly without ever calling router.Route(), so
// a session-affinity request never got its x-aibrix-session-id response header set. A client
// that started its session while only one pod was ready had nothing to echo back on later
// requests, so once the deployment scaled out, session affinity broke on exactly the pods that
// most needed it.
func Test_selectTargetPod_SessionAffinitySurvivesSingleReadyPodFastPath(t *testing.T) {
	routing.Init()

	readyPod := func(name, ip string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: v1.PodStatus{
				PodIP:      ip,
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}

	podA := readyPod("pod-a", "10.0.0.1")
	server := &Server{}

	// First request: only one ready pod, so selectTargetPod takes the fast path.
	routeCtx1 := types.NewRoutingContext(context.Background(), routing.RouterSessionAffinity, "test-model", "hello", "req-1", "user")
	addr1, err := server.selectTargetPod(context.Background(), routeCtx1, &utils.PodArray{Pods: []*v1.Pod{podA}}, "")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:8000", addr1)

	// The fast path must still run session-affinity's post-route hook, so the client gets a
	// session id to echo back later.
	sessionID, ok := routeCtx1.RespHeaders[constants.HeaderSessionID]
	require.True(t, ok, "session-affinity fast path must still set the x-aibrix-session-id response header")
	decoded, decodeErr := base64.StdEncoding.DecodeString(sessionID)
	require.NoError(t, decodeErr)
	assert.Equal(t, addr1, string(decoded))

	// The deployment scales out to a second pod. The client echoes back the session id it was
	// given, and the session must stay pinned to the original pod even though selectTargetPod
	// now goes through the normal (non-fast-path) router.Route() call with two ready pods.
	podB := readyPod("pod-b", "10.0.0.2")
	routeCtx2 := types.NewRoutingContext(context.Background(), routing.RouterSessionAffinity, "test-model", "hello again", "req-2", "user")
	routeCtx2.ReqHeaders = map[string]string{constants.HeaderSessionID: sessionID}
	addr2, err := server.selectTargetPod(context.Background(), routeCtx2, &utils.PodArray{Pods: []*v1.Pod{podA, podB}}, "")
	require.NoError(t, err)
	assert.Equal(t, addr1, addr2, "session must remain pinned to the original pod after scale-out")
}

// TestSelectTargetPod_CandidateStageBoundsRedisReads routes least-request requests over 200
// ready pods against a real Redis protocol server and counts the commands the routing path
// sends. With candidate.k=0 (explicit opt-out), every ready pod's running count is read once
// per decision (the gate, least-request and the load-balance blend share that read, #3329).
// By default (AIBRIX_ROUTING_CANDIDATE_K=64) and with a smaller routingConfig.candidate.k,
// only the candidates are read, so the command count stops depending on the fleet size.
func TestSelectTargetPod_CandidateStageBoundsRedisReads(t *testing.T) {
	// Pin the process default K=64 so this test does not depend on AIBRIX_ROUTING_CANDIDATE_K
	// in the caller's environment.
	restoreDefaults := types.DefaultRoutingOverrides()
	pinned := *restoreDefaults
	pinned.Candidate.K = 64
	types.SetDefaultRoutingOverrides(&pinned)
	t.Cleanup(func() { types.SetDefaultRoutingOverrides(restoreDefaults) })

	const n = 200
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%03d", i), Namespace: "ns"},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.%d.%d", i/256, i%256),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	store := cache.InitWithPods(cache.New(client, nil, nil), pods, "m")
	rm := routing.NewRouterManagerWithCache(store)
	rm.Init()
	s := &Server{cache: store, routerManager: rm}

	route := func(routingConfig string) int {
		t.Helper()
		ctx := types.NewRoutingContext(context.Background(), routing.RouterLeastRequest, "m", "hello", "req-"+routingConfig, "")
		t.Cleanup(ctx.Delete)
		if routingConfig != "" {
			raw := json.RawMessage(routingConfig)
			ctx.ConfigProfile = &types.ResolvedConfigProfile{RoutingConfig: raw, Routing: configprofiles.ParseRoutingConfig(raw)}
		}
		before := mr.CommandCount()
		addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.NoError(t, err)
		require.NotEmpty(t, addr)
		return mr.CommandCount() - before
	}

	full := route(`{"candidate":{"k":0}}`)
	defaultSampled := route("")
	sampled := route(`{"candidate":{"k":8}}`)
	t.Logf("Redis commands per request over %d pods: %d with k=0, %d with default (k=64), %d with k=8", n, full, defaultSampled, sampled)
	assert.GreaterOrEqual(t, full, n, "with k=0 opt-out every ready pod is read")
	assert.GreaterOrEqual(t, defaultSampled, 64, "with default k=64 all 64 candidates are read")
	assert.LessOrEqual(t, defaultSampled, 64+16, "with default k=64 only the 64 candidates are read")
	assert.LessOrEqual(t, sampled, 8+8, "with k=8 only the 8 candidates are read")
}
