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
	"fmt"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
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

func NewNamespaceNameMetricByPa(pa autoscalingv1alpha1.PodAutoscaler) metrics.NamespaceNameMetric {
	return metrics.NewNamespaceNameMetric(pa.Namespace, pa.Spec.ScaleTargetRef.Name, pa.Spec.TargetMetric)
}
