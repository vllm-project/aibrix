package metrics

import (
	"context"
	"fmt"
	autoscaling "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1beta1"
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

func GetPodContainerMetric(ctx context.Context, resource v1.ResourceName, container string) (PodMetricsInfo, error) {
	res := make(PodMetricsInfo, len(rawMetrics))
	for _, m := range rawMetrics {
		containerFound := false
		for _, c := range m.Containers {
			if c.Name == container {
				containerFound = true
				if val, resFound := c.Usage[resource]; resFound {
					res[m.Name] = PodMetric{
						Timestamp: m.Timestamp.Time,
						Window:    m.Window.Duration,
						Value:     val.MilliValue(),
					}
				}
				break
			}
		}
		if !containerFound {
			return nil, fmt.Errorf("container %s not present in metrics for pod %s/%s", container, m.Namespace, m.Name)
		}
	}
	return res, nil
}

func getPodMetrics(ctx context.Context, rawMetrics []metricsapi.PodMetrics, resource v1.ResourceName) PodMetricsInfo {
	res := make(PodMetricsInfo, len(rawMetrics))

	for _, m := range rawMetrics {
		podSum := int64(0)
		missing := len(m.Containers) == 0
		for _, c := range m.Containers {
			resValue, found := c.Usage[resource]
			if !found {
				missing = true
				klog.FromContext(ctx).V(2).Info("Missing resource metric", "resourceMetric", resource, "pod", klog.KRef(m.Namespace, m.Name))
				break
			}
			podSum += resValue.MilliValue()
		}
		if !missing {
			res[m.Name] = PodMetric{
				Timestamp: m.Timestamp.Time,
				Window:    m.Window.Duration,
				Value:     podSum,
			}
		}
	}
	return res
}

func GetObjectMetric(metricName string, namespace string, objectRef *autoscaling.CrossVersionObjectReference, metricSelector labels.Selector) (int64, time.Time, error) {
	gvk := schema.FromAPIVersionAndKind(objectRef.APIVersion, objectRef.Kind)
	var metricValue *customapi.MetricValue
	var err error
	if gvk.Kind == "Namespace" && gvk.Group == "" {
		// handle namespace separately
		// NB: we ignore namespace name here, since CrossVersionObjectReference isn't
		// supposed to allow you to escape your namespace
		metricValue, err = c.client.RootScopedMetrics().GetForObject(gvk.GroupKind(), namespace, metricName, metricSelector)
	} else {
		metricValue, err = c.client.NamespacedMetrics(namespace).GetForObject(gvk.GroupKind(), objectRef.Name, metricName, metricSelector)
	}

	if err != nil {
		return 0, time.Time{}, fmt.Errorf("unable to fetch metrics from custom metrics API: %v", err)
	}

	return metricValue.Value.MilliValue(), metricValue.Timestamp.Time, nil
	panic("not implemented")
}
