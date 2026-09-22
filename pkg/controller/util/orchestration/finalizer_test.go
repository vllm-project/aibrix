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

package orchestration

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRemoveFinalizerRefreshesObjectBeforeUpdate(t *testing.T) {
	const ownedFinalizer = "example.com/owned"
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	stored := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "object", Namespace: "default", Finalizers: []string{ownedFinalizer},
	}}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).Build()
	stale := stored.DeepCopy()
	stale.Finalizers = append(stale.Finalizers, metav1.FinalizerDeleteDependents)

	require.NoError(t, RemoveFinalizer(context.Background(), cli, stale, ownedFinalizer))
	latest := &corev1.ConfigMap{}
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(stored), latest))
	require.Empty(t, latest.Finalizers)
}
