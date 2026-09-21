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

package e2eframework

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestUpdatePodLabelsRetriesConflictsAndRestoresValues(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mock-pod",
			Namespace: "test",
			Labels: map[string]string{
				"existing": "original",
			},
		},
	})
	var failNextUpdate atomic.Bool
	client.Fake.PrependReactor("update", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !failNextUpdate.Swap(false) {
			return false, nil, nil
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "pods"}, "mock-pod", errors.New("conflict"),
		)
	})

	t.Run("update and restore", func(t *testing.T) {
		failNextUpdate.Store(true)
		UpdatePodLabels(t, context.Background(), client, "test", "mock-pod", map[string]string{
			"existing":  "changed",
			"temporary": "value",
		})
		pod, err := client.CoreV1().Pods("test").Get(context.Background(), "mock-pod", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "changed", pod.Labels["existing"])
		require.Equal(t, "value", pod.Labels["temporary"])

		// Registered after UpdatePodLabels, so this runs immediately before its
		// cleanup and makes the restore path retry one resource-version conflict.
		t.Cleanup(func() { failNextUpdate.Store(true) })
	})

	pod, err := client.CoreV1().Pods("test").Get(context.Background(), "mock-pod", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "original", pod.Labels["existing"])
	require.NotContains(t, pod.Labels, "temporary")
}
