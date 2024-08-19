package metrics

import (
	"context"
	v1 "k8s.io/api/core/v1"
	"time"

	autoscaling "k8s.io/api/autoscaling/v2"
)

// PodMetric contains pod metric value (the metric values are expected to be the metric as a milli-value)
type PodMetric struct {
	Timestamp       time.Time
	Window          time.Duration
	Value           int64
	MetricsName     string
	containerPort   int32
	ScaleObjectName string
}

// PodMetricsInfo contains pod metrics as a map from pod names to PodMetricsInfo
type PodMetricsInfo map[string]PodMetric

// MetricsClient knows how to query a remote interface to retrieve container-level
// resource metrics as well as pod-level arbitrary metrics
type MetricsClient interface {
	// GetPodContainerMetric gets the given resource metric (and an associated oldest timestamp)
	// for the specified named container in specific pods in the given namespace and when
	// the container is an empty string it returns the sum of all the container metrics.
	GetPodContainerMetric(ctx context.Context, metricName string, pod v1.Pod, containerPort int) (PodMetricsInfo, time.Time, error)

	// GetObjectMetric gets the given metric (and an associated timestamp) for the given
	// object in the given namespace, it can be used to fetch any object metrics supports /scale interface
	GetObjectMetric(ctx context.Context, metricName string, namespace string, objectRef *autoscaling.CrossVersionObjectReference, containerPort int) (PodMetricsInfo, time.Time, error)
}
