/*
Copyright 2024 The Aibrix Team.

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

package scaler

import (
	"context"
	"fmt"
	"time"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
	podutil "github.com/aibrix/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (a *Autoscaler) getReadyPodsCount(namespace string, selector labels.Selector) (int64, error) {
	podList := &v1.PodList{}
	if err := a.resourceClient.List(context.Background(), podList,
		&client.ListOptions{Namespace: namespace, LabelSelector: selector}); err != nil {
		return 0, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	if len(podList.Items) == 0 {
		return 0, fmt.Errorf("no pods returned by selector while calculating replica count")
	}

	readyPodCount := 0

	for _, pod := range podList.Items {
		if pod.Status.Phase == v1.PodRunning && podutil.IsPodReady(&pod) {
			readyPodCount++
		}
	}

	return int64(readyPodCount), nil
}

func groupPods(pods []*v1.Pod, metrics metrics.PodMetricsInfo, resource v1.ResourceName, cpuInitializationPeriod, delayOfInitialReadinessStatus time.Duration) (readyPodCount int, unreadyPods, missingPods, ignoredPods sets.Set[string]) {
	missingPods = sets.New[string]()
	unreadyPods = sets.New[string]()
	ignoredPods = sets.New[string]()

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
	return readyPodCount, unreadyPods, missingPods, ignoredPods
}

func GetReadyPodsCount(ctx context.Context, podLister client.Client, namespace string, selector labels.Selector) (int64, error) {
	podList, err := podutil.GetPodListByLabelSelector(ctx, podLister, namespace, selector)
	if err != nil {
		return 0, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	return podutil.CountReadyPods(podList)
}

func calculatePodRequests(pods []*v1.Pod, container string, resource v1.ResourceName) (map[string]int64, error) {
	requests := make(map[string]int64, len(pods))
	for _, pod := range pods {
		podSum := int64(0)
		// Calculate all regular containers and restartable init containers requests.
		containers := append([]v1.Container{}, pod.Spec.Containers...)
		for _, c := range pod.Spec.InitContainers {
			if c.RestartPolicy != nil && *c.RestartPolicy == v1.ContainerRestartPolicyAlways {
				containers = append(containers, c)
			}
		}
		for _, c := range containers {
			if container == "" || container == c.Name {
				if containerRequest, ok := c.Resources.Requests[resource]; ok {
					podSum += containerRequest.MilliValue()
				} else {
					return nil, fmt.Errorf("missing request for %s in container %s of Pod %s", resource, c.Name, pod.ObjectMeta.Name)
				}
			}
		}
		requests[pod.Name] = podSum
	}
	return requests, nil
}

func removeMetricsForPods(metrics metrics.PodMetricsInfo, pods sets.Set[string]) {
	for _, pod := range pods.UnsortedList() {
		delete(metrics, pod)
	}
}
