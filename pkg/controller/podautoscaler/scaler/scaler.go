package scaler

import (
	"fmt"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
	podutil "github.com/aibrix/aibrix/pkg/utils/pod"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"time"
)

func groupPods(pods []*v1.Pod, metrics metrics.PodMetricsInfo, resource v1.ResourceName, cpuInitializationPeriod, delayOfInitialReadinessStatus time.Duration) (readyPodCount int, unreadyPods, missingPods, ignoredPods sets.String) {
	missingPods = sets.NewString()
	unreadyPods = sets.NewString()
	ignoredPods = sets.NewString()

	for _, pod := range pods {
		if pod.DeletionTimestamp != nil || pod.Status.Phase == v1.PodFailed {
			ignoredPods.Insert(pod.Name)
			continue
		}
		// Pending pods are ignored.
		if pod.Status.Phase == v1.PodPending {
			unreadyPods.Insert(pod.Name)
			continue
		}
		// Pods missing metrics.
		metric, found := metrics[pod.Name]
		if !found {
			missingPods.Insert(pod.Name)
			continue
		}
		// Unready pods are ignored.
		if resource == v1.ResourceCPU {
			var unready bool
			_, condition := podutil.GetPodCondition(&pod.Status, v1.PodReady)
			if condition == nil || pod.Status.StartTime == nil {
				unready = true
			} else {
				// Pod still within possible initialisation period.
				if pod.Status.StartTime.Add(cpuInitializationPeriod).After(time.Now()) {
					// Ignore sample if pod is unready or one window of metric wasn't collected since last state transition.
					unready = condition.Status == v1.ConditionFalse || metric.Timestamp.Before(condition.LastTransitionTime.Time.Add(metric.Window))
				} else {
					// Ignore metric if pod is unready and it has never been ready.
					unready = condition.Status == v1.ConditionFalse && pod.Status.StartTime.Add(delayOfInitialReadinessStatus).After(condition.LastTransitionTime.Time)
				}
			}
			if unready {
				unreadyPods.Insert(pod.Name)
				continue
			}
		}
		readyPodCount++
	}
	return
}

func (c *Scaler) getReadyPodsCount(namespace string, selector labels.Selector) (int64, error) {
	podList, err := c.podLister.Pods(namespace).List(selector)
	if err != nil {
		return 0, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	if len(podList) == 0 {
		return 0, fmt.Errorf("no pods returned by selector while calculating replica count")
	}

	readyPodCount := 0

	for _, pod := range podList {
		if pod.Status.Phase == v1.PodRunning && podutil.IsPodReady(pod) {
			readyPodCount++
		}
	}

	return int64(readyPodCount), nil
}
