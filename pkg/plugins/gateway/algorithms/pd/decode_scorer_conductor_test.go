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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func makeDecodePod(name, namespace string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func makeDecodeRoutingContext() *types.RoutingContext {
	return &types.RoutingContext{
		Model: testModelName,
	}
}

// ============================================================
// ConductorDecodePolicy: Name / Describe
// ============================================================

func TestConductorDecodePolicy_NameAndDescribe(t *testing.T) {
	p := ConductorDecodePolicy{}
	name := p.Name()
	assert.Equal(t, DecodePolicyConductor, name)
	d := p.Describe()
	assert.NotEmpty(t, d)
	assert.Contains(t, d, "conductor")
}

// ============================================================
// ConductorDecodePolicy: ScoreDecodePod — basic TBT estimation
// ============================================================

func TestConductorDecodePolicy_BasicTBT(t *testing.T) {
	p := ConductorDecodePolicy{}
	ctx := makeDecodeRoutingContext()
	pod := makeDecodePod(testPodName, testNamespace)

	in := DecodePodInput{
		RunningReqs:     4.0,
		Throughput:      50.0,
		FreeGPUPercent:  5.0,
		MaxRequestCount: 10.0,
		MaxThroughput:   100.0,
		MaxFreeGPUUsage: 90.0,
	}

	score := p.ScoreDecodePod(ctx, pod, in)

	// tokenGap = 1000 / 50 = 20ms
	// estimatedTBT = (runningReqs + 1) * tokenGap = (4 + 1) * 20 = 100ms
	// gpuCacheUsage = 1 - 5/100 = 0.95 > 0.9 -> penalty x1.5
	// final = 100 * 1.5 = 150ms
	assert.InDelta(t, 150.0, score, 1e-9)
}

// ============================================================
// ConductorDecodePolicy: ScoreDecodePod — zero throughput fallback
// ============================================================

func TestConductorDecodePolicy_ZeroThroughputFallback(t *testing.T) {
	p := ConductorDecodePolicy{}
	ctx := makeDecodeRoutingContext()
	pod := makeDecodePod(testPodName, testNamespace)

	in := DecodePodInput{
		RunningReqs:     2.0,
		Throughput:      0.0,
		FreeGPUPercent:  50.0,
		MaxRequestCount: 10.0,
		MaxThroughput:   100.0,
		MaxFreeGPUUsage: 90.0,
	}

	score := p.ScoreDecodePod(ctx, pod, in)

	// fallback = DefaultDecodeTBTFallbackMs = 20ms
	// estimatedTBT = (runningReqs + 1) * fallback = (2 + 1) * 20 = 60ms
	// gpuCacheUsage = 0.5 < 0.9 -> no penalty
	assert.InDelta(t, 60.0, score, 1e-9)
}

// ============================================================
// ConductorDecodePolicy: ScoreDecodePod — zero running reqs uses floor of 1
// ============================================================

func TestConductorDecodePolicy_ZeroRunningReqsFloor(t *testing.T) {
	p := ConductorDecodePolicy{}
	ctx := makeDecodeRoutingContext()
	pod := makeDecodePod(testPodName, testNamespace)

	in := DecodePodInput{
		RunningReqs:     0.0,
		Throughput:      50.0,
		FreeGPUPercent:  50.0,
		MaxRequestCount: 10.0,
		MaxThroughput:   100.0,
		MaxFreeGPUUsage: 90.0,
	}

	score := p.ScoreDecodePod(ctx, pod, in)

	// runningReqs = max(0, 1) = 1
	// tokenGap = 1000 / 50 = 20ms
	// estimatedTBT = (1 + 1) * 20 = 40ms
	assert.InDelta(t, 40.0, score, 1e-9)
}

// ============================================================
// ConductorDecodePolicy: ScoreDecodePod — prefers lightly loaded pod
// ============================================================

func TestConductorDecodePolicy_PrefersLightlyLoadedPod(t *testing.T) {
	p := ConductorDecodePolicy{}
	ctx := makeDecodeRoutingContext()
	pod := makeDecodePod(testPodName, testNamespace)

	// Pod A: lightly loaded (1 request running, 50 tok/s)
	// Score: (1 + 1) * (1000 / 50) = 40ms
	scoreA := p.ScoreDecodePod(ctx, pod, DecodePodInput{
		RunningReqs:    1.0,
		Throughput:     50.0,
		FreeGPUPercent: 50.0,
	})

	// Pod B: heavily loaded (40 requests running, 800 tok/s)
	// Score: (40 + 1) * (1000 / 800) = 41 * 1.25 = 51.25ms
	scoreB := p.ScoreDecodePod(ctx, pod, DecodePodInput{
		RunningReqs:    40.0,
		Throughput:     800.0,
		FreeGPUPercent: 50.0,
	})

	assert.InDelta(t, 40.0, scoreA, 1e-9)
	assert.InDelta(t, 51.25, scoreB, 1e-9)
	assert.Less(t, scoreA, scoreB, "lightly-loaded pod must have lower (better) score than busy pod")

	// Same with zero throughput fallback
	scoreA0 := p.ScoreDecodePod(ctx, pod, DecodePodInput{
		RunningReqs:    1.0,
		Throughput:     0.0,
		FreeGPUPercent: 50.0,
	})
	scoreB0 := p.ScoreDecodePod(ctx, pod, DecodePodInput{
		RunningReqs:    40.0,
		Throughput:     0.0,
		FreeGPUPercent: 50.0,
	})
	assert.InDelta(t, 40.0, scoreA0, 1e-9)
	assert.InDelta(t, 820.0, scoreB0, 1e-9)
	assert.Less(t, scoreA0, scoreB0, "lightly-loaded pod must have lower (better) score under fallback")
}
