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
	"github.com/vllm-project/aibrix/pkg/metrics"
)

func TestProcessInFlightMetricLabelContract(t *testing.T) {
	// The pod-local preStop hook requires one series, regardless of model or
	// tenant. Check the real Process emissions so a new label cannot silently
	// make every terminating Pod wait for the full hook timeout.
	originalInc := metrics.IncGaugeMetricFnForTest
	originalDec := metrics.DecGaugeMetricFnForTest
	t.Cleanup(func() {
		metrics.IncGaugeMetricFnForTest = originalInc
		metrics.DecGaugeMetricFnForTest = originalDec
	})
	var updates []int
	capture := func(delta int) func(string, string, []string, ...string) {
		return func(name, help string, labels []string, values ...string) {
			if name != metrics.GatewayInFlight {
				return
			}
			require.Equal(t, []string{"gateway_pod"}, labels)
			require.Equal(t, []string{metrics.GatewayPodName()}, values)
			updates = append(updates, delta)
		}
	}
	metrics.IncGaugeMetricFnForTest = capture(1)
	metrics.DecGaugeMetricFnForTest = capture(-1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&Server{}).Process(newBlockingProcessServer(ctx))
	require.Error(t, err)
	require.Equal(t, []int{1, -1}, updates)
}
