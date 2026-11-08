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

package routeutil

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

type recordReader struct {
	cache.Cache
	record cache.MetricRecord
	err    error
}

func (r recordReader) GetMetricRecordByPod(_, _, _ string) (cache.MetricRecord, error) {
	return r.record, r.err
}
func (r recordReader) GetMetricRecordByPodModel(_, _, _, _ string) (cache.MetricRecord, error) {
	return r.record, r.err
}

func TestFreshKVUsageDiagnostics(t *testing.T) {
	state := klog.CaptureState()
	defer state.Restore()
	flags := flag.NewFlagSet("freshness", flag.ContinueOnError)
	klog.InitFlags(flags)
	require.NoError(t, flags.Set("v", "4"))
	var logs bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&logs)
	now := time.Now()
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	valid := cache.MetricRecord{Value: &metrics.SimpleMetricValue{Value: 0.2}, Timestamp: now}
	for _, tc := range []struct {
		name, metric, label, reason string
		record                      cache.MetricRecord
		err                         error
		want                        bool
	}{
		{"stale", metrics.KVCacheUsagePerc, "gpu", "stale", cache.MetricRecord{Value: valid.Value, Timestamp: now.Add(-2 * time.Second)}, nil, false},
		{"missing GPU", metrics.KVCacheUsagePerc, "gpu", "missing", cache.MetricRecord{}, &cache.MetricNotFoundError{}, false},
		{"invalid", metrics.CPUCacheUsagePerc, "cpu", "invalid", cache.MetricRecord{}, nil, false},
		{"lookup error", metrics.CPUCacheUsagePerc, "cpu", "lookup_error", cache.MetricRecord{}, errors.New("cache unavailable"), false},
		{"missing CPU is optional", metrics.CPUCacheUsagePerc, "cpu", "missing", cache.MetricRecord{}, &cache.MetricNotFoundError{}, true},
		{"fresh", metrics.KVCacheUsagePerc, "gpu", "stale", valid, nil, true},
		{"custom label bounded", "user-supplied-name", "other", "invalid", cache.MetricRecord{}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			counter := kvMetricRejections.WithLabelValues(tc.reason, tc.label)
			before := testutil.ToFloat64(counter)
			_, ok := FreshKVUsage(recordReader{record: tc.record, err: tc.err}, pod, "model", tc.metric, now, time.Second)
			require.Equal(t, tc.want, ok)
			klog.Flush()
			if tc.want {
				require.Equal(t, before, testutil.ToFloat64(counter))
				require.Empty(t, logs.String())
			} else {
				require.Equal(t, before+1, testutil.ToFloat64(counter))
				for _, field := range []string{"ns/p", tc.metric, tc.reason, "age=", "threshold="} {
					require.Contains(t, logs.String(), field)
				}
			}
		})
	}
}

func TestFreshKVUsageWarnsOnceForLegacyAdapter(t *testing.T) {
	state := klog.CaptureState()
	defer state.Restore()
	legacyKVCacheWarning = sync.Once{}
	defer func() { legacyKVCacheWarning = sync.Once{} }()
	var logs bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(io.Discard)
	klog.SetOutputBySeverity("WARNING", &logs)
	valueOnly := struct{ cache.Cache }{cache.NewForTest()}
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "ns"}}
	for i := 0; i < 2; i++ {
		FreshKVUsage(valueOnly, pod, "model", metrics.KVCacheUsagePerc, time.Now(), time.Second)
	}
	klog.Flush()
	require.Equal(t, 1, strings.Count(logs.String(), "does not implement MetricRecordCache"))
}
