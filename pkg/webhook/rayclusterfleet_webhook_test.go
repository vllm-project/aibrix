/*
Copyright 2026 The Aibrix Team.

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

package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestRayClusterFleetValidateSelector(t *testing.T) {
	tests := []struct {
		name     string
		selector *metav1.LabelSelector
		labels   map[string]string
		wantErr  string
	}{
		{
			name:     "matching labels with extra template label",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ray"}},
			labels:   map[string]string{"app": "ray", "team": "inference"},
		},
		{
			name:     "mismatched labels",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ray"}},
			labels:   map[string]string{"app": "other"},
			wantErr:  "selector does not match template labels",
		},
		{
			name:     "missing template labels",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ray"}},
			wantErr:  "selector does not match template labels",
		},
		{
			name:    "nil selector",
			wantErr: "a non-empty selector is required",
		},
		{
			name:     "empty selector",
			selector: &metav1.LabelSelector{},
			wantErr:  "a non-empty selector is required",
		},
		{
			name:     "invalid selector key",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"/": "ray"}},
			labels:   map[string]string{"/": "ray"},
			wantErr:  "spec.selector",
		},
		{
			name: "matching expression",
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"ray", "other"},
			}}},
			labels: map[string]string{"app": "ray"},
		},
		{
			name: "mismatched expression",
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"other"},
			}}},
			labels:  map[string]string{"app": "ray"},
			wantErr: "selector does not match template labels",
		},
		{
			name: "invalid expression",
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "app", Operator: metav1.LabelSelectorOpIn,
			}}},
			labels:  map[string]string{"app": "ray"},
			wantErr: "spec.selector",
		},
	}

	validator := &RayClusterFleetCustomValidator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fleet := &orchestrationapi.RayClusterFleet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-fleet"},
				Spec: orchestrationapi.RayClusterFleetSpec{
					Selector: tt.selector,
					Template: orchestrationapi.RayClusterTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: tt.labels},
					},
				},
			}
			_, createErr := validator.ValidateCreate(context.Background(), fleet)
			_, updateErr := validator.ValidateUpdate(context.Background(), fleet.DeepCopy(), fleet)
			for operation, err := range map[string]error{"create": createErr, "update": updateErr} {
				t.Run(operation, func(t *testing.T) {
					if tt.wantErr == "" {
						require.NoError(t, err)
					} else {
						require.ErrorContains(t, err, tt.wantErr)
						require.True(t, apierrors.IsInvalid(err))
					}
				})
			}
		})
	}
}

func TestRayClusterFleetValidationObjectTypesAndDeletion(t *testing.T) {
	validator := &RayClusterFleetCustomValidator{}
	fleet := &orchestrationapi.RayClusterFleet{}
	_, err := validator.ValidateCreate(context.Background(), &corev1.Pod{})
	require.ErrorContains(t, err, "expected a RayClusterFleet object")
	_, err = validator.ValidateUpdate(context.Background(), fleet, &corev1.Pod{})
	require.ErrorContains(t, err, "expected a RayClusterFleet object")
	_, err = validator.ValidateDelete(context.Background(), fleet)
	require.NoError(t, err)
}
