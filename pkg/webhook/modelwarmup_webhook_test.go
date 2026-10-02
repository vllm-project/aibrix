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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestModelWarmupWebhookValidatesWithoutMaterializingDefaults(t *testing.T) {
	warmup := &modelapi.ModelWarmup{
		Spec: modelapi.ModelWarmupSpec{
			Targets: []modelapi.ModelWarmupTarget{{Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{"node-a"}}}},
			ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
				Image: "busybox:1.36", Command: []string{"sh", "-c", "exit 0"},
			}}},
		},
	}
	w := &ModelWarmupWebhook{}
	_, err := w.ValidateCreate(context.Background(), warmup)
	require.NoError(t, err)
	require.Nil(t, warmup.Spec.Policies)
	require.Empty(t, warmup.Spec.ImagePreload.Images[0].ImagePullPolicy)

	invalid := warmup.DeepCopy()
	invalid.Spec.Targets = []modelapi.ModelWarmupTarget{{NodeSelector: &metav1.LabelSelector{}}}
	_, err = w.ValidateCreate(context.Background(), invalid)
	require.Error(t, err)
}

func TestModelWarmupWebhookRejectsMoreThanMaximumExplicitNodes(t *testing.T) {
	names := make([]string, modelapi.MaxModelWarmupTargets+1)
	for i := range names {
		names[i] = fmt.Sprintf("node-%d", i)
	}
	warmup := validModelWarmupForWebhookTest()
	warmup.Spec.Targets = []modelapi.ModelWarmupTarget{{
		Nodes: &modelapi.ModelWarmupNodesTarget{Names: names},
	}}

	w := &ModelWarmupWebhook{}
	_, err := w.ValidateCreate(context.Background(), warmup)
	require.ErrorContains(t, err, "must have at most 1000 items")
}

func TestModelWarmupWebhookRejectsInvalidLabelSelector(t *testing.T) {
	warmup := validModelWarmupForWebhookTest()
	warmup.Spec.Targets = []modelapi.ModelWarmupTarget{{NodeSelector: &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: "pool", Operator: metav1.LabelSelectorOperator("Invalid"), Values: []string{"a"},
		}},
	}}}

	_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
	require.ErrorContains(t, err, "nodeSelector")
}

func TestModelWarmupWebhookBoundsDeclaredWork(t *testing.T) {
	tests := map[string]func(*modelapi.ModelWarmup){
		"target entries": func(w *modelapi.ModelWarmup) {
			w.Spec.Targets = make([]modelapi.ModelWarmupTarget, 33)
			for i := range w.Spec.Targets {
				w.Spec.Targets[i].NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a"}}
			}
		},
		"declared node names including duplicates": func(w *modelapi.ModelWarmup) {
			w.Spec.Targets[0].Nodes.Names = make([]string, modelapi.MaxModelWarmupTargets+1)
			for i := range w.Spec.Targets[0].Nodes.Names {
				w.Spec.Targets[0].Nodes.Names[i] = "node-a"
			}
		},
		"images": func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images = make([]modelapi.ModelWarmupImage, 33)
			for i := range w.Spec.ImagePreload.Images {
				w.Spec.ImagePreload.Images[i] = modelapi.ModelWarmupImage{
					Image: fmt.Sprintf("image-%d", i), Command: []string{"true"},
				}
			}
		},
		"pull secrets": func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.PullSecrets = make([]corev1.LocalObjectReference, 33)
			for i := range w.Spec.ImagePreload.PullSecrets {
				w.Spec.ImagePreload.PullSecrets[i].Name = fmt.Sprintf("secret-%d", i)
			}
		},
		"command elements": func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images[0].Command = make([]string, 65)
		},
		"argument elements": func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images[0].Args = make([]string, 65)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			warmup := validModelWarmupForWebhookTest()
			mutate(warmup)
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.Error(t, err)
		})
	}
}

func TestModelWarmupWebhookRejectsInvalidPoliciesAndAllowsZeroRetries(t *testing.T) {
	tests := map[string]func(*modelapi.ModelWarmupPolicies){
		"parallelism": func(p *modelapi.ModelWarmupPolicies) { p.Parallelism = ptr.To[int32](0) },
		"job timeout": func(p *modelapi.ModelWarmupPolicies) {
			p.JobTimeoutSeconds = ptr.To[int64](-1)
		},
		"finished job TTL": func(p *modelapi.ModelWarmupPolicies) {
			p.TTLSecondsAfterFinished = ptr.To[int32](-1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			warmup := validModelWarmupForWebhookTest()
			w := &ModelWarmupWebhook{}
			warmup.Spec.Policies = &modelapi.ModelWarmupPolicies{}
			mutate(warmup.Spec.Policies)
			_, err := w.ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, "must be greater than zero")
		})
	}
	warmup := validModelWarmupForWebhookTest()
	warmup.Spec.Policies = &modelapi.ModelWarmupPolicies{RetryLimit: ptr.To[int32](-1)}
	_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
	require.ErrorContains(t, err, "must be greater than or equal to zero")

	warmup = validModelWarmupForWebhookTest()
	warmup.Spec.Policies = &modelapi.ModelWarmupPolicies{RetryLimit: ptr.To[int32](0)}
	_, err = (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
	require.NoError(t, err)
}

func TestModelWarmupWebhookRejectsModeAndSpecUpdates(t *testing.T) {
	w := &ModelWarmupWebhook{}
	invalidMode := validModelWarmupForWebhookTest()
	invalidMode.Spec.Mode = modelapi.ModelWarmupMode("Continuous")
	_, err := w.ValidateCreate(context.Background(), invalidMode)
	require.ErrorContains(t, err, "Unsupported value")

	oldWarmup := validModelWarmupForWebhookTest()
	updated := oldWarmup.DeepCopy()
	updated.Spec.ImagePreload.Images[0].Image = "busybox:latest"
	_, err = w.ValidateUpdate(context.Background(), oldWarmup, updated)
	require.ErrorContains(t, err, "immutable")

	statusOnly := oldWarmup.DeepCopy()
	statusOnly.Status.Phase = modelapi.ModelWarmupRunning
	_, err = w.ValidateUpdate(context.Background(), oldWarmup, statusOnly)
	require.NoError(t, err)
}

func validModelWarmupForWebhookTest() *modelapi.ModelWarmup {
	return &modelapi.ModelWarmup{Spec: modelapi.ModelWarmupSpec{
		Targets: []modelapi.ModelWarmupTarget{{
			Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{"node-a"}},
		}},
		ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
			Image: "busybox:1.36", Command: []string{"sh", "-c", "exit 0"},
		}}},
	}}
}
