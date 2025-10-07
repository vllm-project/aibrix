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

package podautoscaler

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	podutils "github.com/vllm-project/aibrix/pkg/utils"
)

// extractLabelSelector extracts a LabelSelector from the given scale object.
func extractLabelSelector(scale *unstructured.Unstructured) (labels.Selector, error) {
	// Retrieve the selector string from the Scale object's 'spec' field.
	selectorMap, found, err := unstructured.NestedMap(scale.Object, "spec", "selector")
	if err != nil {
		return nil, fmt.Errorf("failed to get 'spec.selector' from scale: %v", err)
	}
	if !found {
		return nil, fmt.Errorf("the 'spec.selector' field was not found in the scale object")
	}

	// Convert selectorMap to a *metav1.LabelSelector object
	selector := &metav1.LabelSelector{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(selectorMap, selector)
	if err != nil {
		return nil, fmt.Errorf("failed to convert 'spec.selector' to LabelSelector: %v", err)
	}

	labelsSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("failed to convert LabelSelector to labels.Selector: %v", err)
	}

	return labelsSelector, nil
}

// GetReadyPodsCount counts the number of ready pods matching the given selector
func GetReadyPodsCount(ctx context.Context, client client.Client, namespace string, selector labels.Selector) (int, error) {
	podList, err := podutils.GetPodListByLabelSelector(ctx, client, namespace, selector)
	if err != nil {
		return 0, fmt.Errorf("failed to list pods: %w", err)
	}

	readyCount := 0
	for _, pod := range podList.Items {
		if isPodReady(pod) {
			readyCount++
		}
	}
	return readyCount, nil
}

// isPodReady checks if a pod is ready
func isPodReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func minFloat64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func boolToCond(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

type ValidationResult struct {
	Valid   bool
	Reason  string
	Message string
}

func invalid(reason, msg string) ValidationResult {
	return ValidationResult{Valid: false, Reason: reason, Message: msg}
}

func validOK() ValidationResult { return ValidationResult{Valid: true} }
