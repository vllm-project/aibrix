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

package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// FetchSingleModelRequestLoad reads running and waiting gauges from one HTTP
// response. Missing or ambiguous series are errors, never an idle sample. Unlike
// the routing-oriented gauge aggregation, it must not let a last zero hide another
// worker's positive gauge. It supports one model and one total per gauge, including
// SGLang's priority="" totals, and intentionally performs no retries or caching.
// This is an observation, not a barrier against requests admitted after the scrape.
func (ef *EngineMetricsFetcher) FetchSingleModelRequestLoad(ctx context.Context, endpoint, engineType string) (float64, error) {
	families, err := ef.fetchAllMetricsFromURL(ctx, fmt.Sprintf("http://%s/%s", endpoint, metricsPathForEngine(engineType)))
	if err != nil {
		return 0, err
	}
	var model string
	var total float64
	for _, name := range []string{NumRequestsRunning, NumRequestsWaiting} {
		rawName, ok := Metrics[name].EngineMetricsNameMapping[engineType]
		if !ok {
			return 0, fmt.Errorf("engine %s does not expose %s", engineType, name)
		}
		family := families[rawName]
		if family == nil {
			return 0, fmt.Errorf("missing request metric %s", rawName)
		}
		instances := filterPriorityMetricInstances(family.Metric, rawName, true)
		if len(instances) != 1 || instances[0].Gauge == nil {
			return 0, fmt.Errorf("request metric %s requires one unambiguous gauge total", rawName)
		}
		if priority, err := GetLabelValueForKey(instances[0], "priority"); err == nil && priority != "" {
			return 0, fmt.Errorf("request metric %s has a priority breakdown without a total", rawName)
		}
		observedModel, err := GetLabelValueForKey(instances[0], "model_name")
		if err != nil || observedModel == "" || (model != "" && model != observedModel) {
			return 0, errors.New("request metrics require the same single model_name")
		}
		model = observedModel
		value := instances[0].Gauge.GetValue()
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return 0, fmt.Errorf("invalid request gauge %s", rawName)
		}
		total += value
	}
	if math.IsInf(total, 0) {
		return 0, errors.New("request load overflow")
	}
	return total, nil
}
