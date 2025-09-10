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

package utils

import (
	"fmt"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCloneAndAddLabel(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		labelKey    string
		labelValue  string
		expected    map[string]string
		shouldClone bool
	}{
		{
			name:        "add label to existing map",
			labels:      map[string]string{"existing": "value"},
			labelKey:    "new",
			labelValue:  "newvalue",
			expected:    map[string]string{"existing": "value", "new": "newvalue"},
			shouldClone: true,
		},
		{
			name:        "add label to empty map",
			labels:      map[string]string{},
			labelKey:    "key",
			labelValue:  "value",
			expected:    map[string]string{"key": "value"},
			shouldClone: true,
		},
		{
			name:        "add label to nil map",
			labels:      nil,
			labelKey:    "key",
			labelValue:  "value",
			expected:    map[string]string{"key": "value"},
			shouldClone: true,
		},
		{
			name:        "overwrite existing label",
			labels:      map[string]string{"key": "oldvalue"},
			labelKey:    "key",
			labelValue:  "newvalue",
			expected:    map[string]string{"key": "newvalue"},
			shouldClone: true,
		},
		{
			name:        "empty label key returns original map",
			labels:      map[string]string{"existing": "value"},
			labelKey:    "",
			labelValue:  "value",
			expected:    map[string]string{"existing": "value"},
			shouldClone: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalLabels := make(map[string]string)
			for k, v := range tt.labels {
				originalLabels[k] = v
			}

			result := CloneAndAddLabel(tt.labels, tt.labelKey, tt.labelValue)

			// Check the result matches expected
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("CloneAndAddLabel() = %v, want %v", result, tt.expected)
			}

			// Check if the map was cloned correctly.
			if tt.labels != nil {
				resultIsSameRef := fmt.Sprintf("%p", result) == fmt.Sprintf("%p", tt.labels)
				if tt.shouldClone && resultIsSameRef {
					t.Errorf("Expected a cloned map, but got the same map reference")
				}
				if !tt.shouldClone && !resultIsSameRef {
					t.Errorf("Expected the same map reference, but got a cloned map")
				}
			}
		})
	}
}

func TestCloneAndRemoveLabel(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		labelKey    string
		expected    map[string]string
		shouldClone bool
	}{
		{
			name:        "remove existing label",
			labels:      map[string]string{"keep": "value", "remove": "removevalue"},
			labelKey:    "remove",
			expected:    map[string]string{"keep": "value"},
			shouldClone: true,
		},
		{
			name:        "remove non-existing label",
			labels:      map[string]string{"keep": "value"},
			labelKey:    "nonexistent",
			expected:    map[string]string{"keep": "value"},
			shouldClone: true,
		},
		{
			name:        "remove from empty map",
			labels:      map[string]string{},
			labelKey:    "key",
			expected:    map[string]string{},
			shouldClone: true,
		},
		{
			name:        "remove from nil map",
			labels:      nil,
			labelKey:    "key",
			expected:    map[string]string{},
			shouldClone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalLabels := make(map[string]string)
			for k, v := range tt.labels {
				originalLabels[k] = v
			}

			result := CloneAndRemoveLabel(tt.labels, tt.labelKey)

			// Check the result matches expected
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("CloneAndRemoveLabel() = %v, want %v", result, tt.expected)
			}

			// Check if original map was modified
			if tt.shouldClone {
				if !reflect.DeepEqual(tt.labels, originalLabels) && tt.labels != nil {
					t.Errorf("Original map was unexpectedly modified")
				}
			} else {
				// When not cloning, should return the same reference
				if &result != &tt.labels {
					t.Errorf("Expected same reference when not cloning")
				}
			}
		})
	}
}

func TestAddLabel(t *testing.T) {
	tests := []struct {
		name       string
		labels     map[string]string
		labelKey   string
		labelValue string
		expected   map[string]string
	}{
		{
			name:       "add label to existing map",
			labels:     map[string]string{"existing": "value"},
			labelKey:   "new",
			labelValue: "newvalue",
			expected:   map[string]string{"existing": "value", "new": "newvalue"},
		},
		{
			name:       "add label to empty map",
			labels:     map[string]string{},
			labelKey:   "key",
			labelValue: "value",
			expected:   map[string]string{"key": "value"},
		},
		{
			name:       "add label to nil map",
			labels:     nil,
			labelKey:   "key",
			labelValue: "value",
			expected:   map[string]string{"key": "value"},
		},
		{
			name:       "empty label key returns original map",
			labels:     map[string]string{"existing": "value"},
			labelKey:   "",
			labelValue: "value",
			expected:   map[string]string{"existing": "value"},
		},
		{
			name:       "overwrite existing label",
			labels:     map[string]string{"key": "oldvalue"},
			labelKey:   "key",
			labelValue: "newvalue",
			expected:   map[string]string{"key": "newvalue"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := AddLabel(tt.labels, tt.labelKey, tt.labelValue)
			// Check the result matches expected
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("AddLabel() = %v, want %v", result, tt.expected)
			}

			// AddLabel should modify the original map (unless labelKey is empty or map is nil)
			if tt.labelKey != "" && tt.labels != nil {
				if !reflect.DeepEqual(tt.labels, tt.expected) {
					t.Errorf("Original map should have been modified but wasn't")
				}
			}
		})
	}
}

func TestCloneSelectorAndAddLabel(t *testing.T) {
	tests := []struct {
		name        string
		selector    *metav1.LabelSelector
		labelKey    string
		labelValue  string
		expected    *metav1.LabelSelector
		shouldClone bool
	}{
		{
			name: "add label to selector with existing MatchLabels",
			selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"existing": "value"},
			},
			labelKey:   "new",
			labelValue: "newvalue",
			expected: &metav1.LabelSelector{
				MatchLabels: map[string]string{"existing": "value", "new": "newvalue"},
			},
			shouldClone: true,
		},
		{
			name: "add label to selector with nil MatchLabels",
			selector: &metav1.LabelSelector{
				MatchLabels: nil,
			},
			labelKey:   "key",
			labelValue: "value",
			expected: &metav1.LabelSelector{
				MatchLabels: map[string]string{"key": "value"},
			},
			shouldClone: true,
		},
		{
			name: "add label to selector with MatchExpressions",
			selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"existing": "value"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "env",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"dev", "test"},
					},
				},
			},
			labelKey:   "new",
			labelValue: "newvalue",
			expected: &metav1.LabelSelector{
				MatchLabels: map[string]string{"existing": "value", "new": "newvalue"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "env",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"dev", "test"},
					},
				},
			},
			shouldClone: true,
		},
		{
			name: "add label to selector with MatchExpressions with nil Values",
			selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "env",
						Operator: metav1.LabelSelectorOpExists,
						Values:   nil,
					},
				},
			},
			labelKey:   "new",
			labelValue: "newvalue",
			expected: &metav1.LabelSelector{
				MatchLabels: map[string]string{"new": "newvalue"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "env",
						Operator: metav1.LabelSelectorOpExists,
						Values:   nil,
					},
				},
			},
			shouldClone: true,
		},
		{
			name: "empty label key returns original selector",
			selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"existing": "value"},
			},
			labelKey:   "",
			labelValue: "value",
			expected: &metav1.LabelSelector{
				MatchLabels: map[string]string{"existing": "value"},
			},
			shouldClone: false,
		},
		{
			name:       "add label to nil selector",
			selector:   nil,
			labelKey:   "new",
			labelValue: "value",
			expected: &metav1.LabelSelector{
				MatchLabels: map[string]string{"new": "value"},
			},
			shouldClone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CloneSelectorAndAddLabel(tt.selector, tt.labelKey, tt.labelValue)

			// Check the result matches expected
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("CloneSelectorAndAddLabel() = %v, want %v", result, tt.expected)
			}

			// Check if a new reference was returned when cloning was expected
			if tt.shouldClone {
				if tt.selector != nil && result == tt.selector {
					t.Errorf("Expected cloned selector, but got same reference")
				}
			} else {
				// When not cloning, should return the same reference
				if tt.selector != nil && result != tt.selector {
					t.Errorf("Expected same reference when not cloning")
				}
			}
		})
	}
}

func TestCloneSelectorAndAddLabel_DeepCopy(t *testing.T) {
	// Test that modifying the cloned selector doesn't affect the original
	original := &metav1.LabelSelector{
		MatchLabels: map[string]string{"original": "value"},
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "env",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"prod"},
			},
		},
	}

	cloned := CloneSelectorAndAddLabel(original, "new", "newvalue")

	// Modify the cloned selector
	cloned.MatchLabels["original"] = "modified"
	cloned.MatchLabels["another"] = "added"
	cloned.MatchExpressions[0].Values[0] = "modified"
	cloned.MatchExpressions[0].Values = append(cloned.MatchExpressions[0].Values, "added")

	// Original should remain unchanged
	if original.MatchLabels["original"] != "value" {
		t.Errorf("Original MatchLabels was modified")
	}
	if _, exists := original.MatchLabels["another"]; exists {
		t.Errorf("Original MatchLabels was unexpectedly modified")
	}
	if original.MatchExpressions[0].Values[0] != "prod" {
		t.Errorf("Original MatchExpressions Values was modified")
	}
	if len(original.MatchExpressions[0].Values) != 1 {
		t.Errorf("Original MatchExpressions Values length was modified")
	}
}
