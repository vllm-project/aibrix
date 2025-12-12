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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	v1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"k8s.io/metrics/pkg/client/clientset/versioned"
	"k8s.io/utils/ptr"
)

func TestRestMetricsFetcher_FetchPodMetrics(t *testing.T) {
	expectedMetricValue := 30.0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := expfmt.MetricFamilyToText(w, &dto.MetricFamily{
			Name: ptr.To("vllm:kv_cache_usage_perc"),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{
					Gauge: &dto.Gauge{
						Value: ptr.To(expectedMetricValue),
					},
				},
			},
		})
		require.NoError(t, err)
	}))
	defer server.Close()

	ip, port := parseURL(server.URL)
	require.NotEmpty(t, ip)
	require.NotEmpty(t, port)

	fetcher := NewRestMetricsFetcherWithConfig(metrics.EngineMetricsFetcherConfig{
		Timeout:    time.Second,
		MaxRetries: 0,
	})

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-1",
			Labels: map[string]string{
				constants.ModelLabelEngine: "vllm",
			},
		},
		Status: corev1.PodStatus{
			PodIP: ip,
		},
	}
	source := autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.POD,
		TargetMetric:     "kv_cache_usage_perc",
		Port:             port,
	}

	actualMetricValue, err := fetcher.FetchPodMetrics(context.TODO(), pod, source)

	assert.NoError(t, err)
	assert.Equal(t, expectedMetricValue, actualMetricValue)
}

func TestResourceMetricsFetcher_FetchPodMetrics(t *testing.T) {
	expectedMetricValue := 50000.0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		metric := v1beta1.PodMetrics{
			Containers: []v1beta1.ContainerMetrics{
				{
					Name: "cpu",
					Usage: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("30.0"),
					},
				},
				{
					Name: "cpu",
					Usage: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("20.0"),
					},
				},
			},
		}
		err := json.NewEncoder(w).Encode(metric)
		require.NoError(t, err)
	}))
	defer server.Close()

	metricsClient, err := versioned.NewForConfig(&rest.Config{
		Host: server.URL,
	})
	require.NoError(t, err)
	fetcher := NewResourceMetricsFetcher(metricsClient)

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelEngine: "vllm",
			},
		},
	}
	source := autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.RESOURCE,
		TargetMetric:     "cpu",
	}

	actualMetricValue, err := fetcher.FetchPodMetrics(context.TODO(), pod, source)

	assert.NoError(t, err)
	assert.Equal(t, expectedMetricValue, actualMetricValue)
}

func parseURL(rawURL string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", ""
	}
	return u.Hostname(), u.Port()
}
