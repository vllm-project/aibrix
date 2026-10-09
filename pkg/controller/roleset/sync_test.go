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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

func TestCalculateStatusRoleReplicas(t *testing.T) {
	tests := []struct {
		name      string
		replicas  *int32
		wantReady corev1.ConditionStatus
	}{
		// Replicas is optional; an omitted value means no desired pods.
		{name: "omitted replicas", replicas: nil, wantReady: corev1.ConditionTrue},
		{name: "zero replicas", replicas: ptr.To[int32](0), wantReady: corev1.ConditionTrue},
		{name: "one replica without pods", replicas: ptr.To[int32](1), wantReady: corev1.ConditionFalse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
			rs := &orchestrationv1alpha1.RoleSet{
				ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "default"},
				Spec: orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{{Name: "worker", Replicas: tt.replicas}},
				},
			}
			r := &RoleSetReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}

			status, err := r.calculateStatus(context.Background(), rs, nil, nil)

			require.NoError(t, err)
			require.Len(t, status.Roles, 1)
			assert.Equal(t, int32(0), status.Roles[0].ReadyReplicas)
			cond := utils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetReady)
			require.NotNil(t, cond)
			assert.Equal(t, tt.wantReady, cond.Status)
		})
	}
}
