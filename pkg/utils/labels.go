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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Clones the given map and returns a new map with the given key and value added.
// Returns the given map, if labelKey is empty.
func CloneAndAddLabel(labels map[string]string, labelKey, labelValue string) map[string]string {
	if labelKey == "" {
		// Don't need to add a label.
		return labels
	}
	// Clone.
	newLabels := map[string]string{}
	for key, value := range labels {
		newLabels[key] = value
	}
	newLabels[labelKey] = labelValue
	return newLabels
}

// CloneAndRemoveLabel clones the given map and returns a new map with the given key removed.
// Returns the given map, if labelKey is empty.
func CloneAndRemoveLabel(labels map[string]string, labelKey string) map[string]string {
	if labelKey == "" {
		// Don't need to add a label.
		return labels
	}
	// Clone.
	newLabels := map[string]string{}
	for key, value := range labels {
		newLabels[key] = value
	}
	delete(newLabels, labelKey)
	return newLabels
}

// AddLabel returns a map with the given key and value added to the given map.
func AddLabel(labels map[string]string, labelKey, labelValue string) map[string]string {
	if labelKey == "" {
		// Don't need to add a label.
		return labels
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[labelKey] = labelValue
	return labels
}

// Use DeepCopy to clone the selector, or create new one if nil
// Returns the given selector, if labelKey is empty.
func CloneSelectorAndAddLabel(selector *metav1.LabelSelector, labelKey, labelValue string) *metav1.LabelSelector {
	if labelKey == "" {
		// Don't need to add a label.
		return selector
	}

	// Use DeepCopy to clone the selector
	var newSelector *metav1.LabelSelector
	if selector != nil {
		newSelector = selector.DeepCopy()
	} else {
		newSelector = &metav1.LabelSelector{}
	}

	// Initialize MatchLabels if nil
	if newSelector.MatchLabels == nil {
		newSelector.MatchLabels = make(map[string]string)
	}

	// Add the new label
	newSelector.MatchLabels[labelKey] = labelValue

	return newSelector
}
