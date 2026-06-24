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
		Model:     "test-model",
		RequestID: "test-req-1",
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

func TestConductorDecodePolicy_Describe(t *testing.T) {
	p := ConductorDecodePolicy{}
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
	pod := makeDecodePod("pod-1", "default")

	in := DecodePodInput{
		RunningReqs:     4.0,
		Throughput:      50.0,
		FreeGPUPercent:  5.0,
		MaxRequestCount: 10.0,
		MaxThroughput:   100.0,
		MaxFreeGPUUsage: 90.0,
	}

	score := p.ScoreDecodePod(ctx, pod, in)

	// currentTBTMs = 1000 / 50 = 20ms
	// estimatedTBT = 20 * (1 + 1/4) = 20 * 1.25 = 25ms
	// gpuCacheUsage = 1 - 5/100 = 0.95 > 0.9 -> penalty x1.5
	// final = 25 * 1.5 = 37.5ms
	assert.InDelta(t, 37.5, score, 1e-9)
}

// ============================================================
// ConductorDecodePolicy: ScoreDecodePod — zero throughput fallback
// ============================================================

func TestConductorDecodePolicy_ZeroThroughputFallback(t *testing.T) {
	p := ConductorDecodePolicy{}
	ctx := makeDecodeRoutingContext()
	pod := makeDecodePod("pod-1", "default")

	in := DecodePodInput{
		RunningReqs:     2.0,
		Throughput:      0.0,
		FreeGPUPercent:  50.0,
		MaxRequestCount: 10.0,
		MaxThroughput:   100.0,
		MaxFreeGPUUsage: 90.0,
	}

	score := p.ScoreDecodePod(ctx, pod, in)

	// currentTBTMs = DefaultDecodeTBTFallbackMs = 20ms
	// estimatedTBT = 20 * (1 + 1/2) = 20 * 1.5 = 30ms
	// gpuCacheUsage = 0.5 < 0.9 -> no penalty
	assert.InDelta(t, 30.0, score, 1e-9)
}

// ============================================================
// ConductorDecodePolicy: ScoreDecodePod — zero running reqs uses floor of 1
// ============================================================

func TestConductorDecodePolicy_ZeroRunningReqsFloor(t *testing.T) {
	p := ConductorDecodePolicy{}
	ctx := makeDecodeRoutingContext()
	pod := makeDecodePod("pod-1", "default")

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
	// currentTBTMs = 1000 / 50 = 20ms
	// estimatedTBT = 20 * (1 + 1/1) = 20 * 2 = 40ms
	assert.InDelta(t, 40.0, score, 1e-9)
}
