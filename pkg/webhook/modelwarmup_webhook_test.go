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
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestModelWarmupWebhookValidatesContinuousPolicies(t *testing.T) {
	tests := []struct {
		name    string
		mode    modelapi.ModelWarmupMode
		policy  modelapi.ModelWarmupPolicies
		wantErr string
	}{
		{name: "continuous defaults", mode: modelapi.ModelWarmupModeContinuous},
		{name: "continuous zero retries", mode: modelapi.ModelWarmupModeContinuous,
			policy: modelapi.ModelWarmupPolicies{ContinuousRetryLimit: ptr.To[int32](0)}},
		{name: "continuous retry limit", mode: modelapi.ModelWarmupModeContinuous,
			policy: modelapi.ModelWarmupPolicies{ContinuousRetryLimit: ptr.To[int32](10)}},
		{name: "negative continuous retry limit", mode: modelapi.ModelWarmupModeContinuous,
			policy: modelapi.ModelWarmupPolicies{ContinuousRetryLimit: ptr.To[int32](-1)}, wantErr: "greater than or equal to zero"},
		{name: "excessive continuous retry limit", mode: modelapi.ModelWarmupModeContinuous,
			policy: modelapi.ModelWarmupPolicies{ContinuousRetryLimit: ptr.To[int32](11)}, wantErr: "less than or equal to 10"},
		{name: "zero continuous retry interval", mode: modelapi.ModelWarmupModeContinuous,
			policy: modelapi.ModelWarmupPolicies{ContinuousRetryIntervalSeconds: ptr.To[int64](0)}, wantErr: "greater than zero"},
		{name: "continuous TTL", mode: modelapi.ModelWarmupModeContinuous,
			policy: modelapi.ModelWarmupPolicies{TTLSecondsAfterFinished: ptr.To[int32](60)}, wantErr: "not supported in Continuous mode"},
		{name: "Once continuous retry limit", mode: modelapi.ModelWarmupModeOnce,
			policy: modelapi.ModelWarmupPolicies{ContinuousRetryLimit: ptr.To[int32](1)}, wantErr: "only supported in Continuous mode"},
		{name: "omitted mode continuous retry interval",
			policy: modelapi.ModelWarmupPolicies{ContinuousRetryIntervalSeconds: ptr.To[int64](30)}, wantErr: "only supported in Continuous mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warmup := validModelWarmupForWebhookTest()
			warmup.Spec.Mode = tt.mode
			if !equality.Semantic.DeepEqual(tt.policy, modelapi.ModelWarmupPolicies{}) {
				warmup.Spec.Policies = tt.policy.DeepCopy()
			}
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestModelWarmupWebhookRejectsUnknownModeAndSpecUpdates(t *testing.T) {
	w := &ModelWarmupWebhook{}
	invalidMode := validModelWarmupForWebhookTest()
	invalidMode.Spec.Mode = modelapi.ModelWarmupMode("Unknown")
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

func TestModelWarmupWebhookAcceptsImageAndCustomWorkModes(t *testing.T) {
	tests := map[string]*modelapi.ModelWarmup{
		"image only":  validModelWarmupForWebhookTest(),
		"custom only": validCustomModelWarmupForWebhookTest(),
		"combined": func() *modelapi.ModelWarmup {
			warmup := validModelWarmupForWebhookTest()
			warmup.Spec.Custom = validModelWarmupCustomActionForWebhookTest()
			return warmup
		}(),
	}

	for name, warmup := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.NoError(t, err)
		})
	}
}

func TestModelWarmupWebhookAcceptsEquivalentCustomQuantityUpdates(t *testing.T) {
	for name, setQuantity := range map[string]func(*modelapi.ModelWarmup, resource.Quantity){
		"container memory": func(w *modelapi.ModelWarmup, quantity resource.Quantity) {
			w.Spec.Custom.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: quantity}
		},
		"volume size limit": func(w *modelapi.ModelWarmup, quantity resource.Quantity) {
			w.Spec.Custom.Volumes[0].EmptyDir.SizeLimit = &quantity
		},
	} {
		t.Run(name, func(t *testing.T) {
			oldWarmup := validCustomModelWarmupForWebhookTest()
			setQuantity(oldWarmup, resource.MustParse("1Gi"))
			updated := oldWarmup.DeepCopy()
			setQuantity(updated, resource.MustParse("1073741824"))
			require.True(t, equality.Semantic.DeepEqual(oldWarmup.Spec, updated.Spec))

			w := &ModelWarmupWebhook{}
			_, err := w.ValidateCreate(context.Background(), oldWarmup)
			require.NoError(t, err)
			_, err = w.ValidateUpdate(context.Background(), oldWarmup, updated)
			require.NoError(t, err)
		})
	}
}

func TestModelWarmupWebhookAcceptsEquivalentCustomJSONUpdates(t *testing.T) {
	for name, mutate := range map[string]func(*modelapi.ModelWarmup){
		"map insertion order": func(w *modelapi.ModelWarmup) {
			requests := corev1.ResourceList{}
			requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
			requests[corev1.ResourceCPU] = resource.MustParse("100m")
			w.Spec.Custom.Containers[0].Resources.Requests = requests
		},
		"nil and empty collections": func(w *modelapi.ModelWarmup) {
			w.Spec.Custom.ImagePullSecrets = []corev1.LocalObjectReference{}
			w.Spec.Custom.Containers[0].Env = []corev1.EnvVar{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			oldWarmup := validCustomModelWarmupForWebhookTest()
			requests := corev1.ResourceList{}
			requests[corev1.ResourceCPU] = resource.MustParse("100m")
			requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
			oldWarmup.Spec.Custom.Containers[0].Resources.Requests = requests
			updated := oldWarmup.DeepCopy()
			mutate(updated)
			_, err := (&ModelWarmupWebhook{}).ValidateUpdate(context.Background(), oldWarmup, updated)
			require.NoError(t, err)
		})
	}
}

func TestModelWarmupWebhookRequiresRegularWork(t *testing.T) {
	tests := map[string]*modelapi.ModelWarmup{
		"neither image nor custom container": func() *modelapi.ModelWarmup {
			warmup := validModelWarmupForWebhookTest()
			warmup.Spec.ImagePreload.Images = nil
			return warmup
		}(),
		"init container only": func() *modelapi.ModelWarmup {
			warmup := validCustomModelWarmupForWebhookTest()
			warmup.Spec.Custom.Containers = nil
			return warmup
		}(),
	}

	for name, warmup := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, "spec: Required value")
			require.NotContains(t, err.Error(), "spec.custom.containers")
		})
	}
}

func TestModelWarmupWebhookBoundsCustomFragments(t *testing.T) {
	tests := map[string]func(*modelapi.ModelWarmup){
		"init containers": func(warmup *modelapi.ModelWarmup) {
			warmup.Spec.Custom.InitContainers = make([]corev1.Container, modelapi.MaxModelWarmupCustomInitContainers+1)
			for i := range warmup.Spec.Custom.InitContainers {
				warmup.Spec.Custom.InitContainers[i] = corev1.Container{Name: fmt.Sprintf("init-%d", i), Image: "busybox:1.36"}
			}
		},
		"containers": func(warmup *modelapi.ModelWarmup) {
			warmup.Spec.Custom.Containers = make([]corev1.Container, modelapi.MaxModelWarmupCustomContainers+1)
			for i := range warmup.Spec.Custom.Containers {
				warmup.Spec.Custom.Containers[i] = corev1.Container{Name: fmt.Sprintf("container-%d", i), Image: "busybox:1.36"}
			}
		},
		"volumes": func(warmup *modelapi.ModelWarmup) {
			warmup.Spec.Custom.Volumes = make([]corev1.Volume, modelapi.MaxModelWarmupCustomVolumes+1)
			for i := range warmup.Spec.Custom.Volumes {
				warmup.Spec.Custom.Volumes[i] = corev1.Volume{
					Name:         fmt.Sprintf("volume-%d", i),
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				}
			}
		},
		"image pull secrets": func(warmup *modelapi.ModelWarmup) {
			warmup.Spec.Custom.ImagePullSecrets = make([]corev1.LocalObjectReference, modelapi.MaxModelWarmupCustomPullSecrets+1)
			for i := range warmup.Spec.Custom.ImagePullSecrets {
				warmup.Spec.Custom.ImagePullSecrets[i] = corev1.LocalObjectReference{Name: fmt.Sprintf("secret-%d", i)}
			}
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			warmup := validCustomModelWarmupForWebhookTest()
			mutate(warmup)
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, "Too many")
		})
	}
}

func TestModelWarmupWebhookRejectsInvalidCustomContainerNames(t *testing.T) {
	tests := map[string]struct {
		mutate       func(*modelapi.ModelWarmup)
		expectedPath string
	}{
		"missing init name": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.InitContainers[0].Name = ""
			},
			expectedPath: "spec.custom.initContainers[0].name",
		},
		"invalid regular name": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Containers[0].Name = "invalid_name"
			},
			expectedPath: "spec.custom.containers[0].name",
		},
		"duplicate init and regular names": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Containers[0].Name = warmup.Spec.Custom.InitContainers[0].Name
			},
			expectedPath: "spec.custom.containers[0].name",
		},
		"generated and custom init names collide": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.ImagePreload.Images = validModelWarmupForWebhookTest().Spec.ImagePreload.Images
				warmup.Spec.Custom.InitContainers[0].Name = "image-0"
			},
			expectedPath: "spec.custom.initContainers[0].name",
		},
		"generated and custom regular names collide": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.ImagePreload.Images = validModelWarmupForWebhookTest().Spec.ImagePreload.Images
				warmup.Spec.Custom.Containers[0].Name = "image-0"
			},
			expectedPath: "spec.custom.containers[0].name",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			warmup := validCustomModelWarmupForWebhookTest()
			test.mutate(warmup)
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, test.expectedPath)
		})
	}
}

func TestModelWarmupWebhookRejectsInvalidCustomVolumes(t *testing.T) {
	tests := map[string]struct {
		mutate       func(*modelapi.ModelWarmup)
		expectedPath string
	}{
		"missing name": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Volumes[0].Name = ""
			},
			expectedPath: "spec.custom.volumes[0].name",
		},
		"invalid name": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Volumes[0].Name = "invalid_name"
			},
			expectedPath: "spec.custom.volumes[0].name",
		},
		"duplicate name": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Volumes = append(warmup.Spec.Custom.Volumes, warmup.Spec.Custom.Volumes[0])
			},
			expectedPath: "spec.custom.volumes[1].name",
		},
		"missing source": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Volumes[0].VolumeSource = corev1.VolumeSource{}
			},
			expectedPath: "spec.custom.volumes[0]",
		},
		"multiple sources": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Volumes[0].HostPath = &corev1.HostPathVolumeSource{Path: "/var/lib/models"}
			},
			expectedPath: "spec.custom.volumes[0]",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			warmup := validCustomModelWarmupForWebhookTest()
			test.mutate(warmup)
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, test.expectedPath)
		})
	}
}

func TestModelWarmupWebhookRejectsUndeclaredCustomVolumeReferences(t *testing.T) {
	tests := map[string]struct {
		mutate       func(*modelapi.ModelWarmup)
		expectedPath string
	}{
		"init container mount": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.InitContainers[0].VolumeMounts = []corev1.VolumeMount{{Name: "missing", MountPath: "/cache"}}
			},
			expectedPath: "spec.custom.initContainers[0].volumeMounts[0].name",
		},
		"regular container mount": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "missing", MountPath: "/cache"}}
			},
			expectedPath: "spec.custom.containers[0].volumeMounts[0].name",
		},
		"init container device": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.InitContainers[0].VolumeDevices = []corev1.VolumeDevice{{Name: "missing", DevicePath: "/dev/cache"}}
			},
			expectedPath: "spec.custom.initContainers[0].volumeDevices[0].name",
		},
		"regular container device": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Containers[0].VolumeDevices = []corev1.VolumeDevice{{Name: "missing", DevicePath: "/dev/cache"}}
			},
			expectedPath: "spec.custom.containers[0].volumeDevices[0].name",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			warmup := validCustomModelWarmupForWebhookTest()
			test.mutate(warmup)
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, test.expectedPath)
			require.ErrorContains(t, err, "undeclared volume")
		})
	}
}

func TestModelWarmupWebhookRejectsContainerRestartPolicies(t *testing.T) {
	for name, mutate := range map[string]func(*modelapi.ModelWarmup){
		"always init container": func(w *modelapi.ModelWarmup) {
			w.Spec.Custom.InitContainers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
		},
		"unsupported init container value": func(w *modelapi.ModelWarmup) {
			w.Spec.Custom.InitContainers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicy("Never"))
		},
		"always regular container": func(w *modelapi.ModelWarmup) {
			w.Spec.Custom.Containers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
		},
		"unsupported regular container value": func(w *modelapi.ModelWarmup) {
			w.Spec.Custom.Containers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicy("Never"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			warmup := validCustomModelWarmupForWebhookTest()
			mutate(warmup)
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, "restartPolicy")
		})
	}
}

func TestModelWarmupWebhookRejectsInvalidCustomContainerImagePullPolicies(t *testing.T) {
	tests := map[string]struct {
		mutate       func(*modelapi.ModelWarmup)
		expectedPath string
	}{
		"init container": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.InitContainers[0].ImagePullPolicy = corev1.PullPolicy("Invalid")
			},
			expectedPath: "spec.custom.initContainers[0].imagePullPolicy",
		},
		"regular container": {
			mutate: func(warmup *modelapi.ModelWarmup) {
				warmup.Spec.Custom.Containers[0].ImagePullPolicy = corev1.PullPolicy("Invalid")
			},
			expectedPath: "spec.custom.containers[0].imagePullPolicy",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			warmup := validCustomModelWarmupForWebhookTest()
			test.mutate(warmup)
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, test.expectedPath)
			require.ErrorContains(t, err, "Unsupported value")
		})
	}
}

func TestModelWarmupWebhookRequiresCustomContainerImages(t *testing.T) {
	warmup := validCustomModelWarmupForWebhookTest()
	warmup.Spec.Custom.Containers[0].Image = ""

	_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
	require.ErrorContains(t, err, "spec.custom.containers[0].image")
}

func TestModelWarmupWebhookRejectsInvalidCustomImagePullSecretNames(t *testing.T) {
	tests := map[string]string{
		"missing": "",
		"invalid": "invalid_name",
	}

	for name, secretName := range tests {
		t.Run(name, func(t *testing.T) {
			warmup := validCustomModelWarmupForWebhookTest()
			warmup.Spec.Custom.ImagePullSecrets = []corev1.LocalObjectReference{{Name: secretName}}
			_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
			require.ErrorContains(t, err, "spec.custom.imagePullSecrets[0].name")
		})
	}
}

func TestModelWarmupWebhookAcceptsCustomFragmentBounds(t *testing.T) {
	warmup := validCustomModelWarmupForWebhookTest()
	warmup.Spec.Custom.InitContainers = make([]corev1.Container, modelapi.MaxModelWarmupCustomInitContainers)
	warmup.Spec.Custom.Containers = make([]corev1.Container, modelapi.MaxModelWarmupCustomContainers)
	warmup.Spec.Custom.Volumes = make([]corev1.Volume, modelapi.MaxModelWarmupCustomVolumes)
	warmup.Spec.Custom.ImagePullSecrets = make([]corev1.LocalObjectReference, modelapi.MaxModelWarmupCustomPullSecrets)
	for i := range warmup.Spec.Custom.InitContainers {
		warmup.Spec.Custom.InitContainers[i] = corev1.Container{Name: fmt.Sprintf("init-%d", i), Image: "busybox:1.36"}
		warmup.Spec.Custom.Containers[i] = corev1.Container{Name: fmt.Sprintf("container-%d", i), Image: "busybox:1.36"}
		warmup.Spec.Custom.Volumes[i] = corev1.Volume{
			Name:         fmt.Sprintf("volume-%d", i),
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}
		warmup.Spec.Custom.ImagePullSecrets[i] = corev1.LocalObjectReference{Name: fmt.Sprintf("secret-%d", i)}
	}

	_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
	require.NoError(t, err)
}

func TestModelWarmupWebhookAcceptsDottedCustomImagePullSecretName(t *testing.T) {
	warmup := validCustomModelWarmupForWebhookTest()
	warmup.Spec.Custom.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry.auth"}}

	_, err := (&ModelWarmupWebhook{}).ValidateCreate(context.Background(), warmup)
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

func validCustomModelWarmupForWebhookTest() *modelapi.ModelWarmup {
	return &modelapi.ModelWarmup{Spec: modelapi.ModelWarmupSpec{
		Targets: []modelapi.ModelWarmupTarget{{
			Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{"node-a"}},
		}},
		Custom: validModelWarmupCustomActionForWebhookTest(),
	}}
}

func validModelWarmupCustomActionForWebhookTest() *modelapi.ModelWarmupCustomAction {
	return &modelapi.ModelWarmupCustomAction{
		InitContainers: []corev1.Container{{
			Name: "init-cache", Image: "busybox:1.36", Command: []string{"sh", "-c", "true"},
			VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
		}},
		Containers: []corev1.Container{{
			Name: "download", Image: "aibrix/runtime:latest",
			VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
		}},
		Volumes: []corev1.Volume{{
			Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
	}
}
