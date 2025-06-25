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

package util

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestComputeHash_ChangesWithTemplate(t *testing.T) {
	t1 := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
	}
	t2 := t1.DeepCopy()
	t2.Labels["version"] = "v2"

	hash1 := ComputeHash(t1, nil)
	hash2 := ComputeHash(t2, nil)

	assert.NotEqual(t, hash1, hash2)
}

func TestComputeHash_WithCollisionCount(t *testing.T) {
	template := &corev1.PodTemplateSpec{}
	var c1, c2 int32 = 1, 2
	h1 := ComputeHash(template, &c1)
	h2 := ComputeHash(template, &c2)
	assert.NotEqual(t, h1, h2)
}

func TestGetPodFromTemplate_ClonesMetadataAndSpec(t *testing.T) {
	template := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{"foo": "bar"},
			Annotations: map[string]string{"a": "b"},
			Finalizers:  []string{"finalizer.k8s.io"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
	}
	controller := &corev1.ReplicationController{ObjectMeta: metav1.ObjectMeta{Name: "ctrl"}}
	ref := metav1.NewControllerRef(controller, corev1.SchemeGroupVersion.WithKind("ReplicationController"))

	pod, err := GetPodFromTemplate(template, controller, ref)
	assert.NoError(t, err)
	assert.Equal(t, "ctrl-", pod.GenerateName)
	assert.Equal(t, template.Labels, pod.Labels)
	assert.Equal(t, template.Annotations, pod.Annotations)
	assert.Equal(t, template.Finalizers, pod.Finalizers)
	assert.Equal(t, template.Spec.Containers[0].Name, pod.Spec.Containers[0].Name)
	assert.Equal(t, ref.UID, pod.OwnerReferences[0].UID)
}

func TestGetPodsPrefix_ValidDNSName(t *testing.T) {
	name := "valid-name"
	prefix := getPodsPrefix(name)
	assert.Equal(t, name+"-", prefix)
	assert.NotEmpty(t, validation.IsDNS1123Subdomain(prefix))
}

func TestGetPodsPrefix_InvalidDNSName(t *testing.T) {
	name := strings.Repeat("a", 254) // too long for dash
	prefix := getPodsPrefix(name)
	assert.Equal(t, name, prefix)
	errs := validation.IsDNS1123Subdomain(prefix + "-")
	assert.NotEmpty(t, errs)
}

func TestGetPodsLabelAnnotationFinalizerSet(t *testing.T) {
	spec := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{"k": "v"},
			Annotations: map[string]string{"a": "b"},
			Finalizers:  []string{"f1"},
		},
	}
	assert.Equal(t, labels.Set{"k": "v"}, getPodsLabelSet(spec))
	assert.Equal(t, labels.Set{"a": "b"}, getPodsAnnotationSet(spec))
	assert.Equal(t, []string{"f1"}, getPodsFinalizers(spec))
}

// Ensure prefix validator is same as upstream behavior
func TestValidatePodName(t *testing.T) {
	assert.Empty(t, ValidatePodName("example-pod-", true))
	assert.NotEmpty(t, ValidatePodName("UPPERCASE", false))
}
