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

package roleset

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

func TestEmitTopologyPolicyPendingReplacementEventForOutdatedPod(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	assert.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	replicas := int32(1)
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-roleset",
			Namespace: "default",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-stormservice",
			},
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			TopologyPolicy: &orchestrationv1alpha1.TopologyPolicy{
				Scope: orchestrationv1alpha1.TopologyRoleSetScope,
				Mode:  orchestrationv1alpha1.TopologyPolicyRequired,
				Key:   "kubernetes.io/hostname",
			},
			Roles: []orchestrationv1alpha1.RoleSpec{
				{
					Name:     "decode",
					Replicas: &replicas,
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "decode-0",
			Namespace: roleSet.Namespace,
			Labels: map[string]string{
				constants.RoleSetNameLabelKey: roleSet.Name,
				constants.RoleNameLabelKey:    "decode",
			},
		},
	}

	recorder := record.NewFakeRecorder(1)
	reconciler := &RoleSetReconciler{
		Client:        fake.NewClientBuilder().WithScheme(scheme).WithObjects(roleSet, pod).Build(),
		EventRecorder: recorder,
	}

	assert.NoError(t, reconciler.emitTopologyPolicyPendingReplacementEvent(ctx, roleSet))

	event := <-recorder.Events
	assert.True(t, strings.Contains(event, corev1.EventTypeWarning))
	assert.True(t, strings.Contains(event, TopologyPolicyPendingPodReplacementEventType))
	assert.True(t, strings.Contains(event, "1 active Pod"))
}
