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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

var (
	validPodSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"model.aibrix.ai/name": "base"},
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "tier", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"canary"}},
		},
	}
	// In requires at least one value.
	invalidPodSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "model", Operator: metav1.LabelSelectorOpIn},
		},
	}
)

func newWebhookTestAdapter(selector *metav1.LabelSelector) *modelapi.ModelAdapter {
	return &modelapi.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "lora", Namespace: "default"},
		Spec: modelapi.ModelAdapterSpec{
			ArtifactURL: "huggingface://org/lora",
			PodSelector: selector,
		},
	}
}

func TestModelAdapterValidateCreate_PodSelector(t *testing.T) {
	tests := map[string]struct {
		selector    *metav1.LabelSelector
		expectError bool
	}{
		"valid labels and expressions": {selector: validPodSelector},
		"In without values":            {selector: invalidPodSelector, expectError: true},
		"Exists with values": {
			selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "model", Operator: metav1.LabelSelectorOpExists, Values: []string{"base"}},
				},
			},
			expectError: true,
		},
		"invalid label value": {
			selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"model": "not a value"}},
			expectError: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := (&ModelAdapterWebhook{}).ValidateCreate(context.Background(), newWebhookTestAdapter(tc.selector))
			if tc.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "spec.podSelector")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestModelAdapterValidateUpdate_PodSelector(t *testing.T) {
	tests := map[string]struct {
		oldSelector *metav1.LabelSelector
		newSelector *metav1.LabelSelector
		expectError bool
	}{
		"changed to invalid": {oldSelector: validPodSelector, newSelector: invalidPodSelector, expectError: true},
		"changed to valid":   {oldSelector: invalidPodSelector, newSelector: validPodSelector},
		// An object stored before validation existed must stay updatable, e.g.
		// so the controller can remove its finalizer.
		"unchanged invalid": {oldSelector: invalidPodSelector, newSelector: invalidPodSelector},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			oldAdapter := newWebhookTestAdapter(tc.oldSelector.DeepCopy())
			newAdapter := newWebhookTestAdapter(tc.newSelector.DeepCopy())
			newAdapter.Finalizers = []string{"adapter.model.aibrix.ai/finalizer"}

			_, err := (&ModelAdapterWebhook{}).ValidateUpdate(context.Background(), oldAdapter, newAdapter)
			if tc.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "spec.podSelector")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
