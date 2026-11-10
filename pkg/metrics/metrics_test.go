/*
Copyright 2025 The Aibrix Team.

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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHistogramMeanQueriesDivideSumByCount guards the queries that average a
// histogram: the numerator must be the histogram's sum and the denominator its
// count. Dividing the sum by itself yields 1 whenever the window has samples
// and NaN when it does not, which is what avg_tpot_pod_5m did.
func TestHistogramMeanQueriesDivideSumByCount(t *testing.T) {
	tests := []string{AvgTTFT5mPod, AvgTPOT5mPod}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			metric, ok := Metrics[name]
			require.True(t, ok, "metric %q is not registered", name)

			parts := strings.Split(metric.PromQL, "/")
			require.Len(t, parts, 2, "metric %q is not a ratio: %s", name, metric.PromQL)
			assert.Contains(t, parts[0], "_sum{", "numerator of %q must be the histogram sum: %s", name, metric.PromQL)
			assert.Contains(t, parts[1], "_count{", "denominator of %q must be the histogram count: %s", name, metric.PromQL)
		})
	}
}
