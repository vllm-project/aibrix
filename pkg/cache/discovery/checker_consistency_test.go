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

package discovery_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
)

// These tests drive the discovery checker's consistency check against a real
// ModelListHealth, the same object /v1/models verifies through.

func newConsistencyChecker(t *testing.T, health *discovery.ModelListHealth) (*cache.DiscoveryChecker, *prometheus.Registry) {
	t.Helper()
	checker, err := cache.NewDiscoveryChecker(cache.DiscoveryCheckOptions{
		Interval: time.Second, UnreachableThreshold: 3 * time.Second,
		Reachability: func(context.Context) error { return nil },
		Consistency:  health,
	})
	require.NoError(t, err)
	reg := prometheus.NewRegistry()
	require.NoError(t, checker.Register(reg))
	return checker, reg
}

func consistencyFailures(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "aibrix_gateway_discovery_verification_failures_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == "check" && label.GetValue() == cache.DiscoveryCheckConsistency {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatal("consistency failure series not found")
	return 0
}

func TestCheckerConsistencyAgeIgnoresReusedProof(t *testing.T) {
	var lists atomic.Int32
	health := discovery.NewTestModelListHealth(time.Minute, func() error { lists.Add(1); return nil })
	checker, _ := newConsistencyChecker(t, health)
	ctx := context.Background()

	checker.CheckConsistencyOnce(ctx)
	require.Equal(t, int32(1), lists.Load())
	time.Sleep(50 * time.Millisecond)

	// The proof is still fresh: this check makes no LIST, so the age must
	// keep counting from the real match instead of resetting.
	checker.CheckConsistencyOnce(ctx)
	assert.Equal(t, int32(1), lists.Load())
	assert.GreaterOrEqual(t, checker.ConsistencyAge(), 50*time.Millisecond)
}

func TestCheckerCountsMatchesFromOtherCallers(t *testing.T) {
	health := discovery.NewTestModelListHealth(time.Minute, func() error { return nil })
	checker, _ := newConsistencyChecker(t, health)
	time.Sleep(50 * time.Millisecond)
	require.GreaterOrEqual(t, checker.ConsistencyAge(), 50*time.Millisecond)

	// A /v1/models request verifies; the gauge reflects that match too.
	require.NoError(t, health.EnsureVerified(context.Background()))
	assert.Less(t, checker.ConsistencyAge(), 50*time.Millisecond)
}

func TestCheckerDoesNotCountAnotherCallersRetryDelay(t *testing.T) {
	var lists atomic.Int32
	health := discovery.NewTestModelListHealth(time.Minute, func() error {
		lists.Add(1)
		return errors.New("connection refused")
	})
	checker, reg := newConsistencyChecker(t, health)
	ctx := context.Background()

	// A /v1/models request fails a verification, which starts a retry delay.
	require.Error(t, health.EnsureVerified(ctx))
	require.Equal(t, int32(1), lists.Load())

	// The checker's tick inside that delay makes no request and must not
	// count a failure it did not observe.
	checker.CheckConsistencyOnce(ctx)
	assert.Equal(t, int32(1), lists.Load())
	assert.Equal(t, 0.0, consistencyFailures(t, reg))
}

func TestCheckerCountsItsOwnFailedVerification(t *testing.T) {
	health := discovery.NewTestModelListHealth(time.Minute, func() error { return errors.New("connection refused") })
	checker, reg := newConsistencyChecker(t, health)
	checker.CheckConsistencyOnce(context.Background())
	assert.Equal(t, 1.0, consistencyFailures(t, reg))
}
