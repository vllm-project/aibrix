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

package routingalgorithms

import (
	"context"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseMultiRouterConfig(t *testing.T) {
	tests := []struct {
		name      string
		routerStr string
		want      *MultiRouterConfig
		wantErr   bool
	}{
		{
			name:      "Single router",
			routerStr: "prefix-cache",
			want: &MultiRouterConfig{
				Items: []RouterItem{
					{Name: "prefix-cache", Coefficient: 1},
				},
			},
			wantErr: false,
		},
		{
			name:      "Multiple routers without weight coefficients (defaults to 1)",
			routerStr: "prefix-cache,least-latency",
			want: &MultiRouterConfig{
				Items: []RouterItem{
					{Name: "prefix-cache", Coefficient: 1},
					{Name: "least-latency", Coefficient: 1},
				},
			},
			wantErr: false,
		},
		{
			name:      "Multiple routers with explicit weight coefficients",
			routerStr: "prefix-cache:6,least-latency:4",
			want: &MultiRouterConfig{
				Items: []RouterItem{
					{Name: "prefix-cache", Coefficient: 6},
					{Name: "least-latency", Coefficient: 4},
				},
			},
			wantErr: false,
		},
		{
			name:      "Multiple routers mixed (with and without weight coefficients)",
			routerStr: "prefix-cache:3,least-latency", // least-latency defaults to 1
			want: &MultiRouterConfig{
				Items: []RouterItem{
					{Name: "prefix-cache", Coefficient: 3},
					{Name: "least-latency", Coefficient: 1},
				},
			},
			wantErr: false,
		},
		{
			name:      "Skip router with 0 weight coefficient",
			routerStr: "prefix-cache:0,least-latency:10",
			want: &MultiRouterConfig{
				Items: []RouterItem{
					{Name: "least-latency", Coefficient: 10},
				},
			},
			wantErr: false,
		},
		{
			name:      "Empty string",
			routerStr: "",
			want:      nil,
			wantErr:   true,
		},
		{
			name:      "Invalid format - empty item",
			routerStr: "prefix-cache,,least-latency",
			want:      nil,
			wantErr:   true,
		},
		{
			name:      "Invalid format - bad separator",
			routerStr: "prefix-cache:1:2",
			want:      nil,
			wantErr:   true,
		},
		{
			name:      "Invalid weight coefficient - float (Gateway API says int32)",
			routerStr: "prefix-cache:0.5,least-latency:0.5",
			want:      nil,
			wantErr:   true,
		},
		{
			name:      "Invalid weight coefficient - negative",
			routerStr: "prefix-cache:-1",
			want:      nil,
			wantErr:   true,
		},
		{
			name:      "Invalid weight coefficient - out of bounds",
			routerStr: "prefix-cache:1000001",
			want:      nil,
			wantErr:   true,
		},
		{
			name:      "All weight coefficients are 0",
			routerStr: "prefix-cache:0,least-latency:0",
			want:      nil,
			wantErr:   true,
		},
		{
			name:      "Exclusive strategy pd ignores others",
			routerStr: "least-request:2,pd:1,throughput:3",
			want: &MultiRouterConfig{
				Items: []RouterItem{
					{Name: "pd", Coefficient: 1},
				},
			},
			wantErr: false,
		},
		{
			name:      "Exclusive strategy slo ignores others",
			routerStr: "slo-least-load:1,least-latency:2",
			want: &MultiRouterConfig{
				Items: []RouterItem{
					{Name: "slo-least-load", Coefficient: 1},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMultiRouterConfig(tt.routerStr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseMultiRouterConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseMultiRouterConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

// simple wrapper to implement PodList for tests
type wrapper struct {
	pods []*v1.Pod
}

func (w wrapper) All() []*v1.Pod { return w.pods }
func (w wrapper) ListPortsForPod() map[string][]int { return nil }
func (w wrapper) Indexes() []string { return nil }
func (w wrapper) ListByIndex(index string) []*v1.Pod { return nil }
func (w wrapper) Len() int { return len(w.pods) }

// Fake scorer for integration testing
type fakeScorer struct {
	scores   map[*v1.Pod]float64
	polarity Polarity
}

func (f *fakeScorer) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	for i, pod := range pods {
		if val, ok := f.scores[pod]; ok {
			if math.IsNaN(val) {
				scored[i] = false
			} else {
				scores[i] = val
				scored[i] = true
			}
		} else {
			scored[i] = false
		}
	}
	return scores, scored, nil
}

func (f *fakeScorer) Polarity() Polarity {
	return f.polarity
}

func TestScoreAndRank(t *testing.T) {
	podA := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "podA"}}
	podB := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "podB"}}
	podC := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "podC"}}
	
	readyPodList := wrapper{[]*v1.Pod{podA, podB, podC}}

	tests := []struct {
		name          string
		cfg           *MultiRouterConfig
		scorers       map[string]PodScorer
		expectedWin   string
		expectedScore map[string]float64
	}{
		{
			name: "1) Single Strategy, HigherIsBetter, normal distribution",
			cfg: &MultiRouterConfig{Items: []RouterItem{{Name: "s1", Coefficient: 1}}},
			scorers: map[string]PodScorer{
				"s1": &fakeScorer{
					polarity: PolarityMost,
					scores: map[*v1.Pod]float64{podA: 10, podB: 20, podC: 30}, // min=10, max=30
				},
			},
			expectedWin: "podC",
			expectedScore: map[string]float64{
				"podA": 0.0, // (10-10)/20
				"podB": 0.5, // (20-10)/20
				"podC": 1.0, // (30-10)/20
			},
		},
		{
			name: "2) Single Strategy, LowerIsBetter, normal distribution",
			cfg: &MultiRouterConfig{Items: []RouterItem{{Name: "s1", Coefficient: 1}}},
			scorers: map[string]PodScorer{
				"s1": &fakeScorer{
					polarity: PolarityLeast,
					scores: map[*v1.Pod]float64{podA: 10, podB: 20, podC: 30}, // min=10, max=30
				},
			},
			expectedWin: "podA",
			expectedScore: map[string]float64{
				"podA": 1.0, // (30-10)/20
				"podB": 0.5, // (30-20)/20
				"podC": 0.0, // (30-30)/20
			},
		},
		{
			name: "3) Multiple Strategies + different weight coefficients",
			cfg: &MultiRouterConfig{Items: []RouterItem{{Name: "s1", Coefficient: 2}, {Name: "s2", Coefficient: 8}}},
			scorers: map[string]PodScorer{
				"s1": &fakeScorer{ // HigherIsBetter
					polarity: PolarityMost,
					scores: map[*v1.Pod]float64{podA: 100, podB: 50, podC: 0}, // A=1.0, B=0.5, C=0.0
				},
				"s2": &fakeScorer{ // LowerIsBetter
					polarity: PolarityLeast,
					scores: map[*v1.Pod]float64{podA: 30, podB: 20, podC: 10}, // A=0.0, B=0.5, C=1.0
				},
			},
			// Total weight = 10
			// A = 1.0*0.2 + 0.0*0.8 = 0.2
			// B = 0.5*0.2 + 0.5*0.8 = 0.5
			// C = 0.0*0.2 + 1.0*0.8 = 0.8
			expectedWin: "podC",
			expectedScore: map[string]float64{
				"podA": 0.2,
				"podB": 0.5,
				"podC": 0.8,
			},
		},
		{
			name: "4) Unscored pods have 0 normalized score, but still participate in other strategies",
			cfg: &MultiRouterConfig{Items: []RouterItem{{Name: "s1", Coefficient: 1}, {Name: "s2", Coefficient: 1}}},
			scorers: map[string]PodScorer{
				"s1": &fakeScorer{ // HigherIsBetter
					polarity: PolarityMost,
					// podC is NaN -> scored=false
					scores: map[*v1.Pod]float64{podA: 100, podB: 50, podC: math.NaN()}, 
					// A=1.0, B=0.0, C=0.0 (penalty)
				},
				"s2": &fakeScorer{ // HigherIsBetter
					polarity: PolarityMost,
					scores: map[*v1.Pod]float64{podA: 10, podB: 20, podC: 30}, 
					// A=0.0, B=0.5, C=1.0
				},
			},
			// Total weight = 2
			// A = 1.0*0.5 + 0.0*0.5 = 0.5
			// B = 0.0*0.5 + 0.5*0.5 = 0.25
			// C = 0.0*0.5 + 1.0*0.5 = 0.5
			// Tie break A and C, original order A is first -> win=A
			expectedWin: "podA",
			expectedScore: map[string]float64{
				"podA": 0.5,
				"podB": 0.25,
				"podC": 0.5,
			},
		},
		{
			name: "5) Strategy with all pods unscored contributes 0 to all pods",
			cfg: &MultiRouterConfig{Items: []RouterItem{{Name: "s1", Coefficient: 1}, {Name: "s2", Coefficient: 1}}},
			scorers: map[string]PodScorer{
				"s1": &fakeScorer{ // HigherIsBetter
					polarity: PolarityMost,
					// All NaN -> scored=false for all -> all 0.0
					scores: map[*v1.Pod]float64{podA: math.NaN(), podB: math.NaN(), podC: math.NaN()}, 
				},
				"s2": &fakeScorer{ // HigherIsBetter
					polarity: PolarityMost,
					scores: map[*v1.Pod]float64{podA: 10, podB: 30, podC: 20}, 
					// A=0.0, B=1.0, C=0.5
				},
			},
			// Total weight = 2
			// A = 0.0*0.5 + 0.0*0.5 = 0.0
			// B = 0.0*0.5 + 1.0*0.5 = 0.5
			// C = 0.0*0.5 + 0.5*0.5 = 0.25
			expectedWin: "podB",
			expectedScore: map[string]float64{
				"podA": 0.0,
				"podB": 0.5,
				"podC": 0.25,
			},
		},
		{
			name: "6) max==min gives normalized score 1.0 (for scored pods only)",
			cfg: &MultiRouterConfig{Items: []RouterItem{{Name: "s1", Coefficient: 1}}},
			scorers: map[string]PodScorer{
				"s1": &fakeScorer{ // HigherIsBetter
					polarity: PolarityMost,
					// A,B are 10. C is NaN.
					scores: map[*v1.Pod]float64{podA: 10, podB: 10, podC: math.NaN()}, 
				},
			},
			// A=1.0, B=1.0, C=0.0
			expectedWin: "podA", // Tie break A, B -> A
			expectedScore: map[string]float64{
				"podA": 1.0,
				"podB": 1.0,
				"podC": 0.0,
			},
		},
		{
			name: "7) Default weight coefficient is 1", // Tested by parsing logic mainly, but we can verify it here if config is provided.
			cfg: &MultiRouterConfig{Items: []RouterItem{{Name: "s1", Coefficient: 1}, {Name: "s2", Coefficient: 3}}},
			scorers: map[string]PodScorer{
				"s1": &fakeScorer{ 
					polarity: PolarityMost,
					scores: map[*v1.Pod]float64{podA: 10, podB: 20, podC: 30}, // A=0, B=0.5, C=1.0
				},
				"s2": &fakeScorer{ 
					polarity: PolarityLeast,
					scores: map[*v1.Pod]float64{podA: 10, podB: 20, podC: 30}, // A=1.0, B=0.5, C=0.0
				},
			},
			// s1 weight=1, s2 weight=3, total=4
			// A = 0.0*0.25 + 1.0*0.75 = 0.75
			// B = 0.5*0.25 + 0.5*0.75 = 0.5
			// C = 1.0*0.25 + 0.0*0.75 = 0.25
			expectedWin: "podA",
			expectedScore: map[string]float64{
				"podA": 0.75,
				"podB": 0.5,
				"podC": 0.25,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &multiStrategyRouter{
				config:  tt.cfg,
				scorers: tt.scorers,
			}

			topPod, scores, err := m.scoreAndRank(&types.RoutingContext{}, readyPodList)
			if err != nil {
				t.Fatalf("scoreAndRank error: %v", err)
			}

			if topPod.Name != tt.expectedWin {
				t.Errorf("Expected winner %s, got %s", tt.expectedWin, topPod.Name)
			}

			for podName, expectedScore := range tt.expectedScore {
				var actualScore float64
				for p, s := range scores {
					if p.Name == podName {
						actualScore = s
						break
					}
				}
				if math.Abs(actualScore-expectedScore) > 1e-9 {
					t.Errorf("Pod %s: expected score %v, got %v", podName, expectedScore, actualScore)
				}
			}
		})
	}
}

func podsFromCache(c *cache.Store) *utils.PodArray {
	return &utils.PodArray{Pods: c.ListPods()}
}

func requestContext(model string) *types.RoutingContext {
	return types.NewRoutingContext(context.Background(), RouterNotSet, model, "", "id", "")
}

func TestSetFallback(t *testing.T) {
	deployment := "deployment"
	store := cache.NewForTest()
	store = cache.InitWithModelRouterProvider(store, NewSLORouter)
	storeCh := cache.InitWithAsyncPods(store, []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployment + "-replicaset-pod1",
				Namespace: "default",
				Labels: map[string]string{
					utils.DeploymentIdentifier: deployment,
				},
			},
			Status: v1.PodStatus{
				PodIP: "1.0.0.1",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
	}, "test model")
	Init() // Required to initialize the router registry after store was initialized and before pods are added.
	store = <-storeCh

	_, err := store.GetRouter(RouterSLO.NewContext(context.Background(), "test model", "", "request id", ""))
	assert.Nil(t, err, "set fallback should wait router initialize")
}

func TestNoPods(t *testing.T) {
	c := cache.Store{}
	r1 := randomRouter{}
	model := ""
	targetPodIP, err := r1.Route(requestContext(model), podsFromCache(&c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r2 := leastRequestRouter{
		cache: &c,
	}
	targetPodIP, err = r2.Route(requestContext(model), podsFromCache(&c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r3 := throughputRouter{
		cache: &c,
	}
	targetPodIP, err = r3.Route(requestContext(model), podsFromCache(&c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")
}

func TestWithNoIPPods(t *testing.T) {
	model := ""
	c := cache.NewWithPodsForTest([]*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p1",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p2",
			},
		},
	}, model)

	r1 := randomRouter{}
	targetPodIP, err := r1.Route(requestContext(model), podsFromCache(c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r3 := throughputRouter{
		cache: c,
	}
	targetPodIP, err = r3.Route(requestContext(model), podsFromCache(c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")
}

func TestWithIPPods(t *testing.T) {
	// two case:
	// case 1: pod ready
	// case 2: pod ready & terminating -> we can send request at this moment.
	model := ""
	c := cache.NewWithPodsMetricsForTest(
		[]*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "p1",
				},
				Status: v1.PodStatus{
					PodIP: "0.0.0.0",
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "p2",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
				Status: v1.PodStatus{
					PodIP: "1.0.0.0",
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
		},
		model,
		map[string]map[string]metrics.MetricValue{
			"p1": {
				metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 15},
				metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 20},
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 20},
			},
			"p2": {
				metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 45},
				metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 15},
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 2},
			},
		})

	pods := podsFromCache(c)
	assert.NotEqual(t, 0, pods.Len(), "No pods initiailized")

	r1 := randomRouter{}
	targetPodIP, err := r1.Route(requestContext(model), pods)
	assert.NoError(t, err)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")

	r2 := leastRequestRouter{
		cache: c,
	}
	targetPodIP, err = r2.Route(requestContext(model), pods)
	assert.NoError(t, err)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")

	r3 := throughputRouter{
		cache: c,
	}
	targetPodIP, err = r3.Route(requestContext(model), pods)
	assert.NoError(t, err)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")
}

// TestSelectRandomPod tests the selectRandomPod function.
func TestSelectRandomPod(t *testing.T) {
	tests := []struct {
		name      string
		pods      []*v1.Pod
		expectErr bool
	}{
		{
			name: "Single ready pod",
			pods: []*v1.Pod{
				{
					Status: v1.PodStatus{
						PodIP: "10.0.0.1",
						Conditions: []v1.PodCondition{
							{
								Type:   v1.PodReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name: "Multiple ready pods",
			pods: []*v1.Pod{
				{
					Status: v1.PodStatus{
						PodIP: "10.0.0.1",
						Conditions: []v1.PodCondition{
							{
								Type:   v1.PodReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
				{
					Status: v1.PodStatus{
						PodIP: "10.0.0.2",
						Conditions: []v1.PodCondition{
							{
								Type:   v1.PodReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
				{
					Status: v1.PodStatus{
						PodIP: "10.0.0.3",
						Conditions: []v1.PodCondition{
							{
								Type:   v1.PodReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name:      "No pods",
			pods:      []*v1.Pod{},
			expectErr: true,
		},
		{
			name: "Pods without IP",
			pods: []*v1.Pod{
				{
					Status: v1.PodStatus{PodIP: ""},
				},
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a new random generator with a fixed seed for consistent test results
			// Seed randomness for consistent results in tests
			r := rand.New(rand.NewSource(42))
			chosenPod, err := utils.SelectRandomPod(tt.pods, r.Intn)
			if tt.expectErr {
				if err == nil {
					t.Errorf("expected an error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				podIP := chosenPod.Status.PodIP
				// Verify that the returned pod IP exists in the input map
				found := false
				for _, pod := range tt.pods {
					if pod.Status.PodIP == podIP {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("returned pod IP %v is not in the input pods", podIP)
				}
			}
		})
	}
}

func TestMultiStrategyRouter_Route(t *testing.T) {
	podA := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "podA"},
		Status: v1.PodStatus{
			PodIP: "1.1.1.1",
			Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue},
			},
		},
	}
	podB := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "podB"},
		Status: v1.PodStatus{
			PodIP: "2.2.2.2",
			Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue},
			},
		},
	}

	readyPodList := wrapper{[]*v1.Pod{podA, podB}}

	scorers := map[string]PodScorer{
		"strategy1": &fakeScorer{
			polarity: PolarityMost,
			scores: map[*v1.Pod]float64{
				podA: 100,
				podB: 50,
			},
		},
	}
	cfg := &MultiRouterConfig{
		Items: []RouterItem{
			{Name: "strategy1", Coefficient: 1},
		},
	}

	m := &multiStrategyRouter{
		config:  cfg,
		scorers: scorers,
	}

	ip, err := m.Route(types.NewRoutingContext(context.Background(), RouterNotSet, "model", "", "id", ""), readyPodList)
	if err != nil {
		t.Fatalf("Route error: %v", err)
	}
	if !strings.Contains(ip, "1.1.1.1") {
		t.Errorf("Expected podA IP to contain 1.1.1.1, got %v", ip)
	}

	// Test no ready pods
	emptyPodList := wrapper{[]*v1.Pod{}}
	_, err = m.Route(types.NewRoutingContext(context.Background(), RouterNotSet, "model", "", "id", ""), emptyPodList)
	if err == nil {
		t.Errorf("Expected error for empty pod list")
	}
}

func TestE2EMultiStrategyRouting(t *testing.T) {
	// Initialize cache with specific metrics for different pods to simulate real environment
	model := "test-model"
	podA := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "podA", Namespace: "default"},
		Status: v1.PodStatus{
			PodIP: "1.1.1.1",
			Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue},
			},
		},
	}
	podB := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "podB", Namespace: "default"},
		Status: v1.PodStatus{
			PodIP: "2.2.2.2",
			Conditions: []v1.PodCondition{
				{Type: v1.PodReady, Status: v1.ConditionTrue},
			},
		},
	}

	c := cache.NewWithPodsMetricsForTest(
		[]*v1.Pod{podA, podB},
		model,
		map[string]map[string]metrics.MetricValue{
			"podA": {
				// least-request wants lower running requests
				metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 10},
				// throughput wants higher throughput
				metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 100},
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 50},
			},
			"podB": {
				metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 2}, // B is better for least-request
				metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 10},  // B is worse for throughput
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 5},
			},
		})

	pods := podsFromCache(c)
	assert.Equal(t, 2, pods.Len())

	// Force initialize router registry using our test cache
	// Normally `Init()` initializes routers using `cache.Get()`. We override the factory specifically for testing here.
	rm := NewRouterManager()
	
	// Register least-request with the test cache
	rm.Register(RouterLeastRequest, func() (types.Router, error) {
		return &leastRequestRouter{cache: c}, nil
	})
	
	// Register throughput with the test cache
	rm.Register(RouterThroughput, func() (types.Router, error) {
		return throughputRouter{cache: c}, nil
	})
	
	// Register session-affinity 
	rm.Register(RouterSessionAffinity, func() (types.Router, error) {
		return NewSessionAffinityRouter()
	})

	// Register prefix-cache
	rm.Register(RouterPrefixCache, func() (types.Router, error) {
		tokenizerObj, _ := tokenizer.NewTokenizer("character", nil)
		return prefixCacheRouter{
			prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(), 
			tokenizer: tokenizerObj,
		}, nil
	})

	rm.Init()

	// 1. E2E Test: Least Request dominates (Weight 10 vs 1)
	t.Run("E2E_MultiStrategy_LeastRequest_Dominates", func(t *testing.T) {
		algString := "least-request:10,throughput:1"
		ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(algString), model, "", "req1", "")
		
		router, err := rm.Select(ctx)
		assert.NoError(t, err)
		
		_, isMulti := router.(*multiStrategyRouter)
		assert.True(t, isMulti, "Expected multiStrategyRouter")

		targetIP, err := router.Route(ctx, pods)
		assert.NoError(t, err)
		
		// podB should win because least-request has weight 10, and podB has 2 requests vs podA's 10.
		// Normalized scores:
		// least-request: podA=0.0, podB=1.0. Weight=10
		// throughput: podA=1.0, podB=0.0. Weight=1
		// Total: podA = 1, podB = 10 -> podB wins!
		assert.Contains(t, targetIP, "2.2.2.2")
	})

	// 2. E2E Test: Throughput dominates (Weight 10 vs 1)
	t.Run("E2E_MultiStrategy_Throughput_Dominates", func(t *testing.T) {
		algString := "least-request:1,throughput:10"
		ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(algString), model, "", "req2", "")
		
		router, err := rm.Select(ctx)
		assert.NoError(t, err)

		targetIP, err := router.Route(ctx, pods)
		assert.NoError(t, err)
		
		// Total: podA = 10, podB = 1 -> podA wins!
		assert.Contains(t, targetIP, "1.1.1.1")
	})

	// 4. E2E Test: 3 strategies, including one without coefficient (defaults to 1), and one with 0 (ignored)
	t.Run("E2E_MultiStrategy_Three_Strategies_With_Corner_Cases", func(t *testing.T) {
		// least-request has no weight (defaults to 1)
		// throughput has weight 2
		// least-latency has weight 0 (should be parsed out and completely ignored)
		algString := "least-request,throughput:2,least-latency:0"
		ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(algString), model, "", "req4", "")
		
		router, err := rm.Select(ctx)
		assert.NoError(t, err)

		targetIP, err := router.Route(ctx, pods)
		assert.NoError(t, err)
		
		// The active strategies are least-request (weight 1) and throughput (weight 2)
		// least-request: podA=0.0, podB=1.0. Weight=1. Score contribution: A=0, B=1
		// throughput: podA=1.0, podB=0.0. Weight=2. Score contribution: A=2, B=0
		// Total: podA = 2, podB = 1 -> podA wins
		assert.Contains(t, targetIP, "1.1.1.1")
	})

	// 5. E2E Test: Invalid configuration format fallback
	t.Run("E2E_MultiStrategy_Invalid_Format_Fallback", func(t *testing.T) {
		// "invalid-strategy:1" is not registered. It should fallback to the RandomRouter
		algString := "least-request:1,invalid-strategy:1"
		ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(algString), model, "", "req5", "")
		
		router, err := rm.Select(ctx)
		assert.NoError(t, err)
		
		// Should fallback to random router, not multiStrategyRouter
		_, isMulti := router.(*multiStrategyRouter)
		assert.False(t, isMulti, "Expected fallback to non-multiStrategyRouter")
		
		_, isRandom := router.(*randomRouter)
		assert.True(t, isRandom, "Expected fallback to randomRouter")
	})

	// 6. E2E Test: Session Affinity combined with Least Request
	t.Run("E2E_MultiStrategy_SessionAffinity_And_LeastRequest", func(t *testing.T) {
		// Session affinity is highly opinionated - either 1 or 0
		// Weight 10 for session affinity means it will heavily bias towards the session pod
		algString := "session-affinity:10,least-request:1"
		ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(algString), model, "", "req6", "")
		
		router, err := rm.Select(ctx)
		assert.NoError(t, err)
		
		targetIP, err := router.Route(ctx, pods)
		assert.NoError(t, err)
		
		// The active strategies are session-affinity (weight 10) and least-request (weight 1).
		// Request has no session header, so session-affinity gives 0 to all pods.
		// session-affinity: podA=0.0, podB=0.0. Weight=10. Score contribution: A=0.0, B=0.0
		// least-request: podA (1 req) = 0.0, podB (0 req) = 1.0. Weight=1. Score contribution: A=0.0, B=1.0
		// Total: podA = 0.0, podB = 1.0 -> podB wins
		assert.Contains(t, targetIP, "2.2.2.2")
	})

	// 7. E2E Test: Prefix Cache combined with Least Request
	t.Run("E2E_MultiStrategy_PrefixCache_And_LeastRequest", func(t *testing.T) {
		// Prefix cache is 1, least-request is 2
		algString := "prefix-cache:1,least-request:2"
		ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(algString), model, "input text", "req7", "")
		
		router, err := rm.Select(ctx)
		assert.NoError(t, err)
		
		targetIP, err := router.Route(ctx, pods)
		assert.NoError(t, err)
		
		// The active strategies are prefix-cache (weight 1) and least-request (weight 2).
		// We didn't seed any prefix cache, so prefix-cache gives 0.0 to all pods.
		// prefix-cache: podA=0.0, podB=0.0. Weight=1. Score contribution: A=0.0, B=0.0
		// least-request: podA (1 req) = 0.0, podB (0 req) = 1.0. Weight=2. Score contribution: A=0.0, B=2.0
		// Total: podA = 0.0, podB = 2.0 -> podB wins
		assert.Contains(t, targetIP, "2.2.2.2")
	})
}
