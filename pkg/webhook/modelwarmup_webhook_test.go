/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
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

func TestModelWarmupWebhookDefaultsAndValidates(t *testing.T) {
	warmup := &modelapi.ModelWarmup{
		Spec: modelapi.ModelWarmupSpec{
			Targets: []modelapi.ModelWarmupTarget{{Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{"node-a"}}}},
			ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
				Image: "busybox:1.36", Command: []string{"sh", "-c", "exit 0"},
			}}},
		},
	}
	w := &ModelWarmupWebhook{}
	require.NoError(t, w.Default(context.Background(), warmup))
	require.Equal(t, corev1.PullIfNotPresent, warmup.Spec.ImagePreload.Images[0].ImagePullPolicy)
	require.EqualValues(t, modelapi.DefaultModelWarmupParallelism, *warmup.Spec.Policies.Parallelism)
	require.EqualValues(
		t,
		modelapi.DefaultModelWarmupGlobalTimeoutSeconds,
		*warmup.Spec.Policies.GlobalTimeoutSeconds,
	)
	require.EqualValues(t, modelapi.DefaultModelWarmupRetryLimit, *warmup.Spec.Policies.RetryLimit)
	require.EqualValues(
		t,
		modelapi.DefaultModelWarmupTTLSecondsAfterFinished,
		*warmup.Spec.Policies.TTLSecondsAfterFinished,
	)
	_, err := w.ValidateCreate(context.Background(), warmup)
	require.NoError(t, err)

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
	require.NoError(t, w.Default(context.Background(), warmup))
	_, err := w.ValidateCreate(context.Background(), warmup)
	require.ErrorContains(t, err, "must have at most 1000 items")
}

func TestModelWarmupWebhookRejectsEveryNonPositivePolicy(t *testing.T) {
	tests := map[string]func(*modelapi.ModelWarmupPolicies){
		"parallelism": func(p *modelapi.ModelWarmupPolicies) { p.Parallelism = ptr.To[int32](0) },
		"global timeout": func(p *modelapi.ModelWarmupPolicies) {
			p.GlobalTimeoutSeconds = ptr.To[int64](-1)
		},
		"retry limit": func(p *modelapi.ModelWarmupPolicies) { p.RetryLimit = ptr.To[int32](0) },
		"finished job TTL": func(p *modelapi.ModelWarmupPolicies) {
			p.TTLSecondsAfterFinished = ptr.To[int32](-1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			warmup := validModelWarmupForWebhookTest()
			w := &ModelWarmupWebhook{}
			require.NoError(t, w.Default(context.Background(), warmup))
			mutate(warmup.Spec.Policies)
			_, err := w.ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, "must be greater than zero")
		})
	}
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
