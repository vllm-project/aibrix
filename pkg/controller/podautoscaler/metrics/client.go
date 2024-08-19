package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"

	"time"
)

const (
	metricServerDefaultMetricWindow = time.Minute
)

// restMetricsClient is a client which supports fetching
// metrics from the pod metrics prometheus API. In future,
// it can fetch from the ai runtime api directly.
type restMetricsClient struct {
}

func (r restMetricsClient) GetPodContainerMetric(ctx context.Context, metricName string, pod corev1.Pod, containerPort int) (PodMetricsInfo, time.Time, error) {
	panic("not implemented")
}

func (r restMetricsClient) GetObjectMetric(ctx context.Context, metricName string, namespace string, objectRef *autoscalingv2.CrossVersionObjectReference, containerPort int) (PodMetricsInfo, time.Time, error) {
	//TODO implement me
	panic("implement me")
}

func GetMetricsFromPods(pods []corev1.Pod, metricName string, metricsPort int) ([]float64, error) {
	var metrics []float64

	for _, pod := range pods {
		// We should use the primary container port. In future, we can decide whether to use sidecar container's port
		url := fmt.Sprintf("http://%s:%d/metrics", pod.Status.PodIP, metricsPort)

		// scrape metrics
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch metrics from pod %s: %v", pod.Name, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response from pod %s: %v", pod.Name, err)
		}

		metricValue, err := parseMetricFromBody(body, metricName)
		if err != nil {
			return nil, fmt.Errorf("failed to parse metrics from pod %s: %v", pod.Name, err)
		}

		metrics = append(metrics, metricValue)
	}

	return metrics, nil
}

func parseMetricFromBody(body []byte, metricName string) (float64, error) {
	lines := strings.Split(string(body), "\n")

	for _, line := range lines {
		if strings.Contains(line, metricName) {
			// format is `http_requests_total 1234.56`
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return 0, fmt.Errorf("unexpected format for metric %s", metricName)
			}

			// parse to float64
			value, err := strconv.ParseFloat(parts[len(parts)-1], 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse metric value for %s: %v", metricName, err)
			}

			return value, nil
		}
	}
	return 0, fmt.Errorf("metrics %s not found", metricName)
}
