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
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// vllmMetricsTestdata is a vLLM v1 /metrics response (one engine, LoRA enabled), rendered by
// prometheus_client from vLLM's metric definitions plus its process, GC and HTTP series.
const vllmMetricsTestdata = "testdata/vllm_metrics.txt"

// parseBothWays parses text with the full parser and with the line filter.
func parseBothWays(text string, keep map[string]struct{}) (kept []byte, full, filtered map[string]*dto.MetricFamily, fullErr, filteredErr error) {
	var fullParser, filteredParser expfmt.TextParser
	full, fullErr = fullParser.TextToMetricFamilies(strings.NewReader(text))
	filter := familyLineFilter{keepFamilies: keep, seen: map[string]familyState{}}
	kept = filter.apply([]byte(text))
	filtered, filteredErr = filteredParser.TextToMetricFamilies(bytes.NewReader(kept))
	return kept, full, filtered, fullErr, filteredErr
}

func requireSameFamilies(t *testing.T, full, filtered map[string]*dto.MetricFamily, keep map[string]struct{}, msg string) {
	t.Helper()
	for name := range keep {
		require.Equal(t, full[name] != nil, filtered[name] != nil, "%s: presence of %s", msg, name)
		require.True(t, proto.Equal(full[name], filtered[name]), "%s: family %s", msg, name)
	}
	for name, family := range filtered {
		require.True(t, proto.Equal(full[name], family), "%s: unexpected family %s", msg, name)
	}
}

func TestParseFamiliesMatchesFullParseOnEnginePayloads(t *testing.T) {
	vllm, err := os.ReadFile(vllmMetricsTestdata)
	require.NoError(t, err)
	sglangDP := `# HELP sglang:num_running_reqs The number of running requests.
# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{model_name="m",dp_rank="0"} 3
sglang:num_running_reqs{model_name="m",dp_rank="1"} 5
# TYPE sglang:token_usage gauge
sglang:token_usage{model_name="m",dp_rank="0"} 0.5
sglang:token_usage{model_name="m",dp_rank="1"} 0.25
# TYPE sglang:max_total_num_tokens gauge
sglang:max_total_num_tokens{model_name="m",dp_rank="0"} 1000
sglang:max_total_num_tokens{model_name="m",dp_rank="1"} 3000
# TYPE sglang:cache_hit_rate gauge
sglang:cache_hit_rate{model_name="m",dp_rank="0"} 0.5
`
	ef := NewEngineMetricsFetcher()
	for _, tc := range []struct {
		name, engine, text string
		metricNames        []string
	}{
		{"vllm", EngineNameVLLM, string(vllm), ef.getAvailableMetricsForEngine(EngineNameVLLM)},
		{"vllm routing subset", EngineNameVLLM, string(vllm),
			[]string{NumRequestsRunning, KVCacheUsagePerc, TimeToFirstTokenSeconds, MaxLora, KVCacheBlockSize}},
		{"sglang dp attention", EngineNameSGLang, sglangDP, []string{NumRequestsRunning, KVCacheUsagePerc}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := setupMockServer(tc.text, 200, 0)
			defer server.Close()
			keep := rawFamiliesToParse(tc.metricNames, tc.engine)
			full, err := ef.fetchMetricsFromURL(context.Background(), server.URL, "", nil)
			require.NoError(t, err)
			filtered, err := ef.fetchMetricsFromURL(context.Background(), server.URL, "", keep)
			require.NoError(t, err)
			requireSameFamilies(t, full, filtered, keep, tc.name)
			assert.Less(t, len(filtered), len(full), "families outside the read set are skipped")
		})
	}
}

func TestParseFamiliesKeepsUndeclaredAndMalformedLines(t *testing.T) {
	keep := map[string]struct{}{"vllm:num_requests_running": {}}
	for _, text := range []string{
		"OK\n",
		"invalid metrics format\n",
		"<html>error</html>\n",
		"# HELP OK\nOK\n",
		"# TYPE other\nother{x=\"1\" 1\n",
		"# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running 1",
	} {
		_, _, _, fullErr, filteredErr := parseBothWays(text, keep)
		require.Error(t, fullErr, "%q", text)
		require.Equal(t, fullErr, filteredErr, "%q", text)
	}

	// A malformed series of a declared family that is not read no longer fails the scrape.
	text := "# TYPE other gauge\nother{x=\"1\" 1\n# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running 2\n"
	_, _, filtered, fullErr, filteredErr := parseBothWays(text, keep)
	require.Error(t, fullErr)
	require.NoError(t, filteredErr)
	assert.Equal(t, 2.0, filtered["vllm:num_requests_running"].Metric[0].GetGauge().GetValue())
}

// TestParseFamiliesMatchesFullParseOnRandomText compares the filter with the full
// parser on generated text, including suffix collisions, TYPE quirks and syntax errors.
func TestParseFamiliesMatchesFullParseOnRandomText(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	names := []string{"a", "a_sum", "a_count", "a_bucket", "a_total", "b", "b_sum", "b_bucket", "c_count",
		"c", "vllm:x", "vllm:x_bucket", "vllm:x_created", "_sum", "x_count_sum"}
	types := []string{"counter", "gauge", "histogram", "summary", "untyped", "Gauge", "HISTOGRAM", `summ\ary`,
		"gauge ", "bogus", ""}
	pick := func(items []string) string { return items[rng.Intn(len(items))] }
	sample := func(name string) string {
		var labels []string
		if rng.Intn(2) == 0 {
			labels = append(labels, fmt.Sprintf(`model_name="m%d"`, rng.Intn(2)))
		}
		switch rng.Intn(4) {
		case 0:
			labels = append(labels, `le="`+pick([]string{"0.5", "1", "+Inf"})+`"`)
		case 1:
			labels = append(labels, `quantile="`+pick([]string{"0.5", "0.9"})+`"`)
		}
		value := pick([]string{"1", "2.5", "0", "NaN", "+Inf", "17"})
		if len(labels) == 0 {
			return name + " " + value
		}
		return name + "{" + strings.Join(labels, ",") + "} " + value
	}
	line := func() string {
		switch r := rng.Intn(100); {
		case r < 12:
			return "# TYPE " + pick(names) + " " + pick(types)
		case r < 20:
			return "# HELP " + pick(names) + " some \\\\ help \\n text"
		case r < 70:
			return pick([]string{"", " ", "\t"}) + sample(pick(names))
		case r < 74:
			return pick([]string{"", "#", "# a comment", "# HELP", "# TYPE a", "# TYPE  a\tgauge", "  # HELP b x"})
		case r < 77:
			return pick([]string{"{bad} 1", "1abc 2", "a{x=\"1\" 1", "# HELP a{ x", "a 1 2 3", "a\r"})
		default:
			// A well-formed histogram or summary series.
			base := pick([]string{"a", "b", "c", "vllm:x"})
			if rng.Intn(2) == 0 {
				return base + `_bucket{le="1"} 3` + "\n" + base + "_sum 4\n" + base + "_count 3"
			}
			return base + `{quantile="0.5"} 1` + "\n" + base + "_sum 4\n" + base + "_count 3"
		}
	}

	var succeeded, filteredOnly int
	for i := 0; i < 5000; i++ {
		lines := make([]string, 1+rng.Intn(30))
		for j := range lines {
			lines[j] = line()
		}
		text := strings.Join(lines, "\n")
		if rng.Intn(10) != 0 {
			text += "\n"
		}
		keep := map[string]struct{}{}
		for _, name := range names {
			if rng.Intn(3) == 0 {
				keep[name] = struct{}{}
			}
		}
		kept, full, filtered, fullErr, filteredErr := parseBothWays(text, keep)
		msg := fmt.Sprintf("case %d: %q keep=%v", i, text, keep)
		if fullErr == nil {
			require.NoError(t, filteredErr, msg)
			requireSameFamilies(t, full, filtered, keep, msg)
			succeeded++
			continue
		}
		var parseErr expfmt.ParseError
		require.True(t, errors.As(fullErr, &parseErr), msg)
		keptLines := strings.Split(string(kept), "\n")
		if !strings.HasSuffix(text, "\n") && parseErr.Line == len(keptLines) {
			// An unterminated last line is passed through and fails both parses.
			require.Error(t, filteredErr, msg)
			continue
		}
		if parseErr.Line <= len(keptLines) && keptLines[parseErr.Line-1] != "" {
			// The line that fails the full parse reaches the parser and fails it the same way.
			require.Equal(t, fullErr, filteredErr, msg)
			continue
		}
		if filteredErr == nil {
			filteredOnly++
		}
	}
	t.Logf("full parse succeeded in %d cases; %d failed only on skipped lines", succeeded, filteredOnly)
	require.Greater(t, succeeded, 500)
}
