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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A ConditionType represents a condition a resource could be in.
type ConditionType string

// A Condition that may apply to a resource.
type Condition struct {
	// Type of this condition. At most one of each condition type may apply to a resource at any point in time.
	Type ConditionType `json:"type"`

	// Status of this condition; is it currently True, False, or Unknown?
	Status corev1.ConditionStatus `json:"status"`

	// LastTransitionTime is the last time this condition transitioned from one status to another.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// LastUpdateTime is the last time this condition was updated.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// LastUpdateMicroTime is the last time with microsecond level precision this condition was updated.
	// +optional
	LastUpdateMicroTime *metav1.MicroTime `json:"lastUpdateMicroTime,omitempty"`

	// The Reason for the condition's last transition.
	Reason string `json:"reason,omitempty"`

	// A Message containing details about this condition's last transition from one status to another, if any.
	// +optional
	Message string `json:"message,omitempty"`
}

func NewCondition(ct ConditionType, status corev1.ConditionStatus, msg string) Condition {
	return Condition{
		Type:    ct,
		Status:  status,
		Message: msg,
	}
}

// Conditions reflects the observed status of a resource. Only
// one condition of each type may exist.
type Conditions []Condition

// GetCondition returns the condition for the given ConditionType if exists,
// otherwise returns nil
func (s Conditions) GetCondition(ct ConditionType) Condition {
	for _, c := range s {
		if c.Type == ct {
			return c
		}
	}

	return Condition{Type: ct, Status: corev1.ConditionUnknown}
}

// SetConditions sets the supplied conditions, replacing any existing conditions
// of the same type. This is a no-op if all supplied conditions are identical,
// ignoring the last transition time, to those already set.
func (s *Conditions) SetConditions(conditions ...Condition) {
	for _, c := range conditions {
		found := false
		for i := range *s {
			ref := &(*s)[i]
			if ref.Type != c.Type {
				continue
			}

			found = true

			if ref.Equal(&c) {
				if !c.LastUpdateTime.Equal(ref.LastUpdateTime) {
					ref.LastUpdateTime = c.LastUpdateTime
				}
				if !c.LastUpdateMicroTime.Equal(ref.LastUpdateMicroTime) {
					ref.LastUpdateMicroTime = c.LastUpdateMicroTime
				}
				continue
			}
			if ref.Status != c.Status {
				now := metav1.Now()
				c.LastTransitionTime = &now
			}
			*ref = c
		}
		if !found {
			now := metav1.Now()
			c.LastTransitionTime = &now
			*s = append(*s, c)
		}
	}
}

// Equal used to judge weather two condition is equal.
// LastTransitionTime, LastUpdateTime and LastUpdateMicroTime is ignored.
func (c *Condition) Equal(candidate *Condition) bool {
	return c.Message == candidate.Message &&
		c.Type == candidate.Type &&
		c.Status == candidate.Status
}
