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

package roleset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

type notFoundUpdateClient struct {
	client.Client
}

func (c *notFoundUpdateClient) Update(
	_ context.Context,
	obj client.Object,
	_ ...client.UpdateOption,
) error {
	return apierrors.NewNotFound(schema.GroupResource{
		Group:    orchestrationv1alpha1.GroupVersion.Group,
		Resource: "rolesets",
	}, obj.GetName())
}

func TestFinalizeTreatsRoleSetNotFoundAsComplete(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	roleSet := &orchestrationv1alpha1.RoleSet{ObjectMeta: metav1.ObjectMeta{
		Name: "deleted", Namespace: "default", Finalizers: []string{RoleSetFinalizer},
	}}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(roleSet.DeepCopy()).Build()
	reconciler := &RoleSetReconciler{
		Client:        &notFoundUpdateClient{Client: baseClient},
		DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
	}

	done, err := reconciler.finalize(context.Background(), roleSet)
	require.NoError(t, err)
	require.True(t, done)
}
