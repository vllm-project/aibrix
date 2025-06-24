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

package utils

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	NAMESPACE            = "aibrix-system"
	modelPortIdentifier  = "model.aibrix.ai/port"
	defaultPodMetricPort = 8000
)

var (
	ReplicaSetDeploymentFinder = regexp.MustCompile(`^(.*)-\w+$`)     // Deployment-[random name]
	RayClusterFleetFinder      = regexp.MustCompile(`^(.*)-\w+-\w+$`) // RayClusterFleet-[random name]-[random name]
)

var DeploymentIdentifier string = getDeploymentIdentifier()

func getDeploymentIdentifier() string {
	return LoadEnv("AIBRIX_POD_DEPLOYMENT_LABEL", "app.kubernetes.io/name")
}

// GeneratePodKey generates a key in the format "namespace/name" for a given pod.
func GeneratePodKey(podNamespace, podName string) string {
	return fmt.Sprintf("%s/%s", podNamespace, podName)
}

// ParsePodKey parses a key in the format "namespace/podName".
// Returns (namespace, podName, success).
func ParsePodKey(key string) (string, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		klog.V(4).Infof("Invalid key format: %q. Expected format: namespace/name", key)
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsPodTerminating check if pod is in terminating status via whether the deletion timestamp is set
func IsPodTerminating(pod *v1.Pod) bool {
	return pod.ObjectMeta.DeletionTimestamp != nil
}

// IsPodReady returns true if a pod is ready; false otherwise.
func IsPodReady(pod *v1.Pod) bool {
	return IsPodReadyConditionTrue(pod.Status)
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsPodReadyConditionTrue(status v1.PodStatus) bool {
	condition := GetPodReadyCondition(status)
	return condition != nil && condition.Status == v1.ConditionTrue
}

// GetPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetPodReadyCondition(status v1.PodStatus) *v1.PodCondition {
	_, condition := GetPodCondition(&status, v1.PodReady)
	return condition
}

// GetPodCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetPodCondition(status *v1.PodStatus, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return GetPodConditionFromList(status.Conditions, conditionType)
}

// GetPodConditionFromList extracts the provided condition from the given list of condition and
// returns the index of the condition and the condition. Returns -1 and nil if the condition is not present.
func GetPodConditionFromList(conditions []v1.PodCondition, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}

// SetConditionInList sets the specific condition type on the given PodAutoscaler to the specified value with the given
// reason and message.
// The message and args are treated like a format string.
// The condition will be added if it is not present. The new list will be returned.
func SetConditionInList(inputList []metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string, args ...interface{}) []metav1.Condition {
	resList := inputList
	var existingCond *metav1.Condition
	for i, condition := range resList {
		if condition.Type == conditionType {
			// can't take a pointer to an iteration variable
			existingCond = &resList[i]
			break
		}
	}

	if existingCond == nil {
		resList = append(resList, metav1.Condition{
			Type: conditionType,
		})
		existingCond = &resList[len(resList)-1]
	}

	if existingCond.Status != status {
		existingCond.LastTransitionTime = metav1.Now()
	}

	existingCond.Status = status
	existingCond.Reason = reason
	existingCond.Message = fmt.Sprintf(message, args...)

	return resList
}

func GetPodListByLabelSelector(ctx context.Context, podLister client.Client, namespace string, selector labels.Selector) (*v1.PodList, error) {
	podList := &v1.PodList{}
	err := podLister.List(ctx, podList, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to get pods: %v", err)
	}
	return podList, nil
}

func CountReadyPods(podList *v1.PodList) (int64, error) {
	if podList == nil || len(podList.Items) == 0 {
		return 0, nil
	}

	readyPodCount := 0
	for _, pod := range podList.Items {
		isReady := IsPodReady(&pod)
		if pod.Status.Phase == v1.PodRunning && isReady {
			readyPodCount++
		}
		klog.V(4).InfoS("CountReadyPods Pod status", "name", pod.Name, "phase", pod.Status.Phase, "ready", isReady)
	}

	return int64(readyPodCount), nil
}

func FilterReadyPod(pod *v1.Pod) bool {
	return pod.Status.PodIP != "" && !IsPodTerminating(pod) && IsPodReady(pod)
}

// CountRoutablePods filters and returns the number of pods that are routable.
// A pod is routable if it have a valid PodIP and not in terminating state.
func CountRoutablePods(pods []*v1.Pod) (cnt int) {
	for _, pod := range pods {
		if !FilterReadyPod(pod) {
			continue
		}
		cnt++
	}
	return
}

// FilterRoutablePods filters and returns a list of pods that are routable.
// A pod is routable if it have a valid PodIP and not in terminating state.
func FilterRoutablePods(pods []*v1.Pod) []*v1.Pod {
	readyPods := make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		if !FilterReadyPod(pod) {
			continue
		}
		readyPods = append(readyPods, pod)
	}
	return readyPods
}

// FilterRoutablePodsInPlace filters a list of pods that are routable.
// A pod is routable if it have a valid PodIP and not in terminating state.
func FilterRoutablePodsInPlace(pods []*v1.Pod) []*v1.Pod {
	readyCnt := 0
	for i, pod := range pods {
		if !FilterReadyPod(pod) {
			continue
		} else if readyCnt != i {
			pods[readyCnt] = pod
		}
		readyCnt++
	}
	return pods[:readyCnt]
}

// FilterActivePods returns active pods.
func FilterActivePods(pods []v1.Pod) []v1.Pod {
	return FilterPods(pods, FilterReadyPod)
}

type filterPod func(p *v1.Pod) bool

// FilterPods returns replica sets that are filtered by filterFn (all returned ones should match filterFn).
func FilterPods(pods []v1.Pod, filterFn filterPod) []v1.Pod {
	var filtered []v1.Pod
	for i := range pods {
		if filterFn(&pods[i]) {
			filtered = append(filtered, pods[i])
		}
	}
	return filtered
}

// FilterPodByName returns the pod with the given name.
func FilterPodByName(podname string, pods []*v1.Pod) (*v1.Pod, bool) {
	for _, pod := range pods {
		if pod.Name == podname {
			return pod, true
		}
	}
	return nil, false
}

// DeploymentNameFromPod extracts the deployment name from the pod using two methods:
// 1. If the pod has a label with the key "app.kubernetes.io/name", its value is considered the deployment name.
// 2. If the pod has an owner reference of kind "ReplicaSet", the deployment name is extracted from the owner reference's name.
// Alternatively, if the pod is a ray cluster node, we check:
// 1. If the pod has a label with the key "orchestration.aibrix.ai/raycluster-fleet-name", its value is considered the deployment name.
// 2. "app.kubernetes.io/name" is discared for ray cluster node identifid by the label "ray.io/is-ray-node"
// 3. If the pod has an owner reference of kind "RayCluster", the deployment name is extracted from the owner reference's name.
func DeploymentNameFromPod(pod *v1.Pod) string {
	if fleet, ok := pod.Labels[ReyClusterFleetIdentifier]; ok {
		return fleet
	} else if dpName, ok := pod.Labels[DeploymentIdentifier]; ok {
		// double check if RayClusterNodeType is not available
		isRayNode, rayOK := pod.Labels[RayClusterIdentifier]
		if !rayOK || isRayNode != RayClusterIdentifierYes {
			return dpName
		}
	}

	// Try load from ReplicaSet
	ownerReferences := pod.OwnerReferences
	if len(ownerReferences) > 0 {
		for _, ownerRef := range ownerReferences {
			var re *regexp.Regexp
			switch ownerRef.Kind {
			case "ReplicaSet":
				re = ReplicaSetDeploymentFinder
			case "RayCluster":
				re = RayClusterFleetFinder
			default:
				continue
			}

			matches := re.FindStringSubmatch(ownerRef.Name)
			if len(matches) > 1 {
				return matches[1]
			}
		}
	}

	return ""
}

// SelectRandomPod selects a random pod from the provided list, ensuring it's routable.
// It returns an error if no ready pods are available.
func SelectRandomPod(pods []*v1.Pod, randomFn func(int) int) (*v1.Pod, error) {
	readyPods := FilterRoutablePods(pods)
	if len(readyPods) == 0 {
		return nil, fmt.Errorf("no ready pods available for random selection")
	}
	randomPod := readyPods[randomFn(len(readyPods))]
	return randomPod, nil
}

func GetModelPortForPod(requestID string, pod *v1.Pod) int64 {
	value, ok := pod.Labels[modelPortIdentifier]
	if !ok {
		klog.Warningf("requestID: %v, pod: %v is missing port identifier label: %v, hence default to port: %v",
			requestID, pod.Name, modelPortIdentifier, defaultPodMetricPort)
		// if pod.Labels == nil {
		// 	pod.Labels = make(map[string]string)
		// }
		// pod.Labels[modelPortIdentifier] = strconv.Itoa(defaultPodMetricPort)
		return defaultPodMetricPort
	}

	modelPort, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		klog.Warningf("requestID: %v, pod: %v has incorrect value: %v for port identifier label: %v, hence default to port: %v",
			requestID, pod.Name, value, modelPortIdentifier, defaultPodMetricPort)
		modelPort = defaultPodMetricPort
	}
	return modelPort
}
