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
	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func validModelClaimForWebhookTest() *modelapi.ModelClaim {
	return &modelapi.ModelClaim{ObjectMeta: metav1.ObjectMeta{Name: "test-model"}, Spec: modelapi.ModelClaimSpec{
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "warm"}},
		ArtifactURL: "huggingface://org/model", Engine: "vllm",
		PerGPU: &modelapi.ModelClaimPerGPU{MaximumFootprint: resource.MustParse("2Gi"), KVFloor: resource.MustParse("1Gi")},
	}}
}

func TestModelClaimWebhookCreate(t *testing.T) {
	cases := []struct {
		name   string
		change func(*modelapi.ModelClaim)
		field  string
	}{
		{"valid", func(c *modelapi.ModelClaim) {}, ""},
		{"missing perGPU", func(c *modelapi.ModelClaim) { c.Spec.PerGPU = nil }, ""},
		{"empty selector", func(c *modelapi.ModelClaim) { c.Spec.PodSelector = &metav1.LabelSelector{} }, ""},
		{"missing selector", func(c *modelapi.ModelClaim) { c.Spec.PodSelector = nil }, "spec.podSelector"},
		{"invalid label key", func(c *modelapi.ModelClaim) { c.Spec.PodSelector.MatchLabels = map[string]string{"bad key": "a"} }, "spec.podSelector.matchLabels"},
		{"invalid label value", func(c *modelapi.ModelClaim) { c.Spec.PodSelector.MatchLabels["pool"] = "bad value" }, "spec.podSelector.matchLabels"},
		{"invalid selector operator", func(c *modelapi.ModelClaim) {
			c.Spec.PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "pool", Operator: "Invalid"}}
		}, "spec.podSelector.matchExpressions[0].operator"},
		{"invalid selector values", func(c *modelapi.ModelClaim) {
			c.Spec.PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "pool", Operator: metav1.LabelSelectorOpExists, Values: []string{"a"}}}
		}, "spec.podSelector.matchExpressions[0].values"},
		{"empty artifact", func(c *modelapi.ModelClaim) { c.Spec.ArtifactURL = "" }, "spec.artifactURL"},
		{"blank artifact", func(c *modelapi.ModelClaim) { c.Spec.ArtifactURL = " \n\t" }, "spec.artifactURL"},
		{"local artifact", func(c *modelapi.ModelClaim) { c.Spec.ArtifactURL = "/models/weights" }, ""},
		{"bare model", func(c *modelapi.ModelClaim) { c.Spec.ArtifactURL = "org/model" }, ""},
		{"empty model name", func(c *modelapi.ModelClaim) { c.Spec.ModelName = ptr.To("") }, "spec.modelName"},
		{"blank model name", func(c *modelapi.ModelClaim) { c.Spec.ModelName = ptr.To(" \t") }, "spec.modelName"},
		{"served model name", func(c *modelapi.ModelClaim) { c.Spec.ModelName = ptr.To("org/served-model") }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claim := validModelClaimForWebhookTest()
			tc.change(claim)
			before := claim.DeepCopy()
			_, err := (&ModelClaimWebhook{}).ValidateCreate(context.Background(), claim)
			require.Equal(t, before, claim, "validation must not mutate its input")
			if tc.field == "" {
				require.NoError(t, err)
				return
			}
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			var status *apierrors.StatusError
			require.ErrorAs(t, err, &status)
			require.Equal(t, tc.field, status.ErrStatus.Details.Causes[0].Field)
		})
	}
}

func TestModelClaimWebhookPerGPU(t *testing.T) {
	for _, field := range []string{"maximumFootprint", "kvFloor"} {
		for _, value := range []string{"0", "-1Gi", "30m", "1500m", "1048575500m", "2Pi", "7Ei", "1e30"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				claim := validModelClaimForWebhookTest()
				if field == "maximumFootprint" {
					claim.Spec.PerGPU.MaximumFootprint = resource.MustParse(value)
				} else {
					claim.Spec.PerGPU.KVFloor = resource.MustParse(value)
				}
				_, err := (&ModelClaimWebhook{}).ValidateCreate(context.Background(), claim)
				require.True(t, apierrors.IsInvalid(err), "%v", err)
				require.Contains(t, err.Error(), "spec.perGPU."+field)
			})
		}
	}
	for _, value := range []string{"1", "1Mi", "1Pi", "5.6Gi", "1048576500m"} {
		claim := validModelClaimForWebhookTest()
		claim.Spec.PerGPU.MaximumFootprint = resource.MustParse(value)
		claim.Spec.PerGPU.KVFloor = resource.MustParse(value)
		_, err := (&ModelClaimWebhook{}).ValidateCreate(context.Background(), claim)
		require.NoError(t, err, value)
	}
	claim := validModelClaimForWebhookTest()
	claim.Spec.PerGPU = &modelapi.ModelClaimPerGPU{}
	_, err := (&ModelClaimWebhook{}).ValidateCreate(context.Background(), claim)
	var status *apierrors.StatusError
	require.ErrorAs(t, err, &status)
	require.Len(t, status.ErrStatus.Details.Causes, 2)
}

func TestModelClaimWebhookEngineConfig(t *testing.T) {
	for _, tc := range []struct {
		name, engine string
		args         map[string]string
		invalid      bool
	}{
		{"default", "", nil, false},
		{"parallel", "vllm", map[string]string{"--tensor-parallel-size": "2", "--pipeline-parallel-size": "2"}, false},
		{"aliases", "vllm", map[string]string{"-tp=2": "", "--pipeline_parallel_size": "2"}, false},
		{"unknown argument", "vllm", map[string]string{"--future-flag": ""}, false},
		{"zero", "vllm", map[string]string{"--tensor-parallel-size": "0"}, true},
		{"negative", "vllm", map[string]string{"--pipeline-parallel-size": "-1"}, true},
		{"noninteger", "vllm", map[string]string{"--tensor-parallel-size": "two"}, true},
		{"empty", "vllm", map[string]string{"--tensor-parallel-size": ""}, true},
		{"overflow", "vllm", map[string]string{"--tensor-parallel-size": "9223372036854775807", "--pipeline-parallel-size": "2"}, true},
		{"data parallel", "vllm", map[string]string{"-dp": "2"}, true},
		{"gpu utilization", "vllm", map[string]string{"--gpu_memory_utilization=0.9": ""}, true},
		{"duplicate aliases", "vllm", map[string]string{"-tp": "2", "--tensor-parallel-size": "4"}, true},
		{"sglang", "sglang", map[string]string{"--gpu-memory-utilization": "0.9", "--data-parallel-size": "2"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := validModelClaimForWebhookTest()
			claim.Spec.Engine = tc.engine
			claim.Spec.EngineConfig = &modelapi.ModelClaimEngineConfig{Args: tc.args}
			_, err := (&ModelClaimWebhook{}).ValidateCreate(context.Background(), claim)
			if tc.invalid {
				require.True(t, apierrors.IsInvalid(err))
				require.Contains(t, err.Error(), "spec.engineConfig.args")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestModelClaimWebhookUpdateCompatibility(t *testing.T) {
	old := validModelClaimForWebhookTest()
	old.Spec.ArtifactURL = " "
	old.Spec.ModelName = ptr.To("")
	old.Spec.PodSelector.MatchLabels["pool"] = "bad value"
	old.Spec.EngineConfig = &modelapi.ModelClaimEngineConfig{Args: map[string]string{"-tp": "0"}}
	old.Spec.PerGPU.KVFloor = resource.MustParse("0")
	old.Finalizers = []string{"test/finalizer"}
	for _, tc := range []struct {
		name    string
		change  func(*modelapi.ModelClaim)
		invalid bool
	}{
		{"metadata", func(c *modelapi.ModelClaim) { c.Labels = map[string]string{"a": "b"} }, false},
		{"finalizer", func(c *modelapi.ModelClaim) { c.Finalizers = nil }, false},
		{"repair perGPU", func(c *modelapi.ModelClaim) { c.Spec.PerGPU.KVFloor = resource.MustParse("2Gi") }, false},
		{"remove perGPU", func(c *modelapi.ModelClaim) { c.Spec.PerGPU = nil }, false},
		{"new invalid perGPU", func(c *modelapi.ModelClaim) { c.Spec.PerGPU.KVFloor = resource.MustParse("-1Gi") }, true},
		{"new blank artifact", func(c *modelapi.ModelClaim) { c.Spec.ArtifactURL = "\t" }, true},
		{"new blank name", func(c *modelapi.ModelClaim) { c.Spec.ModelName = ptr.To(" ") }, true},
		{"new invalid selector", func(c *modelapi.ModelClaim) { c.Spec.PodSelector.MatchLabels["pool"] = "another bad value" }, true},
		{"new invalid args", func(c *modelapi.ModelClaim) { c.Spec.EngineConfig.Args["-tp"] = "-1" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated := old.DeepCopy()
			tc.change(updated)
			_, err := (&ModelClaimWebhook{}).ValidateUpdate(context.Background(), old, updated)
			if tc.invalid {
				require.True(t, apierrors.IsInvalid(err), "%v", err)
			} else {
				require.NoError(t, err)
			}
		})
	}
	for _, before := range []*modelapi.ModelClaim{validModelClaimForWebhookTest(), {Spec: modelapi.ModelClaimSpec{}}} {
		updated := before.DeepCopy()
		updated.Spec.PerGPU = validModelClaimForWebhookTest().Spec.PerGPU
		_, err := (&ModelClaimWebhook{}).ValidateUpdate(context.Background(), before, updated)
		require.NoError(t, err)
	}
	_, err := (&ModelClaimWebhook{}).ValidateDelete(context.Background(), old)
	require.NoError(t, err)
}
