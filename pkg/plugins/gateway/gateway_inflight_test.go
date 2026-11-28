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
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestModelInFlightTracking(t *testing.T) {
	var gauges []map[string]string
	originalIncFn := metrics.IncGaugeMetricFnForTest
	originalDecFn := metrics.DecGaugeMetricFnForTest
	defer func() {
		metrics.IncGaugeMetricFnForTest = originalIncFn
		metrics.DecGaugeMetricFnForTest = originalDecFn
	}()
	recordFn := func(dir string) func(name string, help string, labelNames []string, labelValues ...string) {
		return func(name string, help string, labelNames []string, labelValues ...string) {
			if name != metrics.GatewayModelInFlight {
				return
			}
			labels := make(map[string]string, len(labelNames))
			for i, ln := range labelNames {
				labels[ln] = labelValues[i]
			}
			labels["_dir"] = dir
			gauges = append(gauges, labels)
		}
	}
	metrics.IncGaugeMetricFnForTest = recordFn("inc")
	metrics.DecGaugeMetricFnForTest = recordFn("dec")

	st := &processState{model: "qwen3-8B"}
	st.trackModelInFlight()
	st.trackModelInFlight() // idempotent
	require.Len(t, gauges, 1)
	require.Equal(t, "qwen3-8B", gauges[0]["model"])
	require.Equal(t, "inc", gauges[0]["_dir"])

	st.releaseModelInFlight()
	st.releaseModelInFlight() // idempotent
	require.Len(t, gauges, 2)
	require.Equal(t, "dec", gauges[1]["_dir"])
}

func TestModelInFlightTracking_LoraAdapter(t *testing.T) {
	var gauges []map[string]string
	originalIncFn := metrics.IncGaugeMetricFnForTest
	originalDecFn := metrics.DecGaugeMetricFnForTest
	defer func() {
		metrics.IncGaugeMetricFnForTest = originalIncFn
		metrics.DecGaugeMetricFnForTest = originalDecFn
	}()
	recordFn := func(dir string) func(name string, help string, labelNames []string, labelValues ...string) {
		return func(name string, help string, labelNames []string, labelValues ...string) {
			if name != metrics.GatewayModelInFlight {
				return
			}
			labels := make(map[string]string, len(labelNames))
			for i, ln := range labelNames {
				labels[ln] = labelValues[i]
			}
			labels["_dir"] = dir
			gauges = append(gauges, labels)
		}
	}
	metrics.IncGaugeMetricFnForTest = recordFn("inc")
	metrics.DecGaugeMetricFnForTest = recordFn("dec")

	st := &processState{
		model:     "hip-adapter",
		routerCtx: &types.RoutingContext{Model: "hip-adapter", BaseModel: "qwen3-8b"},
	}
	st.trackModelInFlight()
	require.Len(t, gauges, 1)
	require.Equal(t, "qwen3-8b", gauges[0]["model"])
	require.Equal(t, "hip-adapter", gauges[0]["lora_adapter"])

	st.releaseModelInFlight()
	require.Len(t, gauges, 2)
	require.Equal(t, "qwen3-8b", gauges[1]["model"])
	require.Equal(t, "hip-adapter", gauges[1]["lora_adapter"])
}

func TestModelInFlightTracking_ModelChange(t *testing.T) {
	var gauges []map[string]string
	originalIncFn := metrics.IncGaugeMetricFnForTest
	originalDecFn := metrics.DecGaugeMetricFnForTest
	defer func() {
		metrics.IncGaugeMetricFnForTest = originalIncFn
		metrics.DecGaugeMetricFnForTest = originalDecFn
	}()
	recordFn := func(dir string) func(name string, help string, labelNames []string, labelValues ...string) {
		return func(name string, help string, labelNames []string, labelValues ...string) {
			if name != metrics.GatewayModelInFlight {
				return
			}
			labels := make(map[string]string, len(labelNames))
			for i, ln := range labelNames {
				labels[ln] = labelValues[i]
			}
			labels["_dir"] = dir
			gauges = append(gauges, labels)
		}
	}
	metrics.IncGaugeMetricFnForTest = recordFn("inc")
	metrics.DecGaugeMetricFnForTest = recordFn("dec")

	st := &processState{model: "inferred-model"}
	st.trackModelInFlight()
	require.Len(t, gauges, 1)
	require.Equal(t, "inferred-model", gauges[0]["model"])
	require.Equal(t, "inc", gauges[0]["_dir"])

	st.model = "actual-model"
	st.trackModelInFlight()
	require.Len(t, gauges, 3)
	require.Equal(t, "inferred-model", gauges[1]["model"])
	require.Equal(t, "dec", gauges[1]["_dir"])
	require.Equal(t, "actual-model", gauges[2]["model"])
	require.Equal(t, "inc", gauges[2]["_dir"])

	st.releaseModelInFlight()
	require.Len(t, gauges, 4)
	require.Equal(t, "actual-model", gauges[3]["model"])
	require.Equal(t, "dec", gauges[3]["_dir"])
}

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
