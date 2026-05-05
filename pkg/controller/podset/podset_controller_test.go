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

package podset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicFake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/record"
	clientFake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

func TestCreatePodFromTemplate_EnvOrder(t *testing.T) {
	// Create a podSet with podGroupSize: 2
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-podset",
			Namespace: "test-namespace",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 2,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Name: "init-container",
							Env: []corev1.EnvVar{
								{Name: "INIT_USER_VAR", Value: "init-value"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Env: []corev1.EnvVar{
								{Name: "USER_VAR_Z", Value: "value-z"},
								{Name: "USER_VAR_A", Value: "value-a"},
							},
						},
					},
				},
			},
		},
	}
	scheme := runtime.NewScheme()
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().Build(),
		Scheme:        scheme,
		EventRecorder: &record.FakeRecorder{},
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	// Call createPodFromTemplate with podIndex 0
	pod, err := reconciler.createPodFromTemplate(podSet, 0)
	assert.NoError(t, err, "createPodFromTemplate should not return error")
	assert.NotNil(t, pod, "pod should not be nil")

	// Verify container count
	assert.Len(t, pod.Spec.Containers, 1, "pod should have one container")
	container := &pod.Spec.Containers[0]

	// Define built-in environment variables
	builtInEnvNames := []string{
		constants.PodSetNameEnvKey,
		constants.PodSetIndexEnvKey,
		constants.PodSetSizeEnvKey,
	}

	// Verify environment variables count
	expectedEnvCount := len(builtInEnvNames) + 2 // 3 built-in + 2 user-defined
	assert.Equal(t, expectedEnvCount, len(container.Env), "container should have correct number of env vars")

	// Verify built-in env vars are at the beginning
	for i := 0; i < len(builtInEnvNames); i++ {
		assert.Equal(t, builtInEnvNames[i], container.Env[i].Name, "Built-in env var should be at the beginning")
	}

	// Verify user-defined env vars maintain their order
	userEnvStartIndex := len(builtInEnvNames)
	expectedUserEnvOrder := []string{"USER_VAR_Z", "USER_VAR_A"}
	for i, expectedName := range expectedUserEnvOrder {
		actualIndex := userEnvStartIndex + i
		assert.Less(t, actualIndex, len(container.Env), "should have enough user-defined env vars")
		assert.Equal(t, expectedName, container.Env[actualIndex].Name, "User-defined env var should maintain original order")
	}

	// Verify InitContainers have built-in env vars at the beginning
	assert.Len(t, pod.Spec.InitContainers, 1, "pod should have one init container")
	initContainer := &pod.Spec.InitContainers[0]
	expectedInitEnvCount := len(builtInEnvNames) + 1 // 3 built-in + 1 user-defined
	assert.Equal(t, expectedInitEnvCount, len(initContainer.Env), "init container should have correct number of env vars")
	// Verify built-in env vars are at the beginning of init container
	for i := 0; i < len(builtInEnvNames); i++ {
		assert.Equal(t, builtInEnvNames[i], initContainer.Env[i].Name, "Built-in env var should be at the beginning of init container")
	}
	// Verify user-defined env var in init container
	assert.Equal(t, "INIT_USER_VAR", initContainer.Env[len(builtInEnvNames)].Name, "User-defined env var should be present in init container")
	assert.Equal(t, "init-value", initContainer.Env[len(builtInEnvNames)].Value, "User-defined env var value should be preserved in init container")
}

func TestCreatePodFromTemplate_EnvConflict(t *testing.T) {
	// Create a podSet with podGroupSize: 2 and conflicting env vars
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-podset",
			Namespace: "test-namespace",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 2,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Env: []corev1.EnvVar{
								{Name: constants.PodSetNameEnvKey, Value: "user-override"}, // Conflict with built-in
								{Name: "USER_VAR", Value: "value"},                         // Non-conflicting
								{Name: constants.PodSetSizeEnvKey, Value: "999"},           // Conflict with built-in
							},
						},
					},
				},
			},
		},
	}
	scheme := runtime.NewScheme()
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().Build(),
		Scheme:        scheme,
		EventRecorder: &record.FakeRecorder{},
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	// Call createPodFromTemplate with podIndex 0
	pod, err := reconciler.createPodFromTemplate(podSet, 0)
	assert.NoError(t, err, "createPodFromTemplate should not return error")
	assert.NotNil(t, pod, "pod should not be nil")

	// Verify container count
	assert.Len(t, pod.Spec.Containers, 1, "pod should have one container")
	container := &pod.Spec.Containers[0]

	// Define built-in environment variables
	builtInEnvNames := []string{
		constants.PodSetNameEnvKey,
		constants.PodSetIndexEnvKey,
		constants.PodSetSizeEnvKey,
	}

	// Verify environment variables count
	expectedEnvCount := len(builtInEnvNames) + 1 // 3 built-in + 1 non-conflicting user-defined
	assert.Equal(t, expectedEnvCount, len(container.Env), "container should have correct number of env vars")

	// Verify built-in env vars are at the beginning and not overridden
	for i := 0; i < len(builtInEnvNames); i++ {
		assert.Equal(t, builtInEnvNames[i], container.Env[i].Name, "Built-in env var should be at the beginning")
	}

	// Only non-conflicting user-defined env var should be present
	env := container.Env[len(builtInEnvNames)]
	assert.NotNil(t, env, "should have non-conflicting user-defined env var")
	assert.Equal(t, "USER_VAR", env.Name, "Only non-conflicting user-defined env var should be present")
	assert.Equal(t, "value", env.Value, "User-defined env var value should be preserved")
}
