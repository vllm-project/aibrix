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

package podautoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/monitor"
)

// TestPooledRoleScaleUpWhilePodsStart runs two scaling passes of the controller against a
// pooled StormService role whose new pods are slow to appear. The first pass scales the role
// from 2 to 6. Fifteen seconds later only the 2 old pods exist and the recommendation dips to
// 3. The scale-down window must hold the template at 6, so the second pass starts its decision
// from the template count. Status keeps reporting the pods that exist as actualScale, so the
// autoscaler still shows it is scaling rather than at its target.
func TestPooledRoleScaleUpWhilePodsStart(t *testing.T) {
	ctx := context.Background()

	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)
	_ = orchestrationv1alpha1.AddToScheme(sch)

	ss := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "test-storm"},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Mode: orchestrationv1alpha1.StormServicePooledMode,
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{{Name: "prefill", Replicas: ptr.To(int32(2))}},
				},
			},
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			RoleStatuses: []orchestrationv1alpha1.RoleStatus{{Name: "prefill", Replicas: 2}},
		},
	}
	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "prefill-pa"},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: orchestrationv1alpha1.GroupVersion.String(),
				Kind:       StormService,
				Name:       ss.Name,
			},
			SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{RoleName: "prefill"},
			MinReplicas:       ptr.To(int32(1)),
			MaxReplicas:       10,
			ScalingStrategy:   autoscalingv1alpha1.KPA,
		},
	}
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(ss).Build()

	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{orchestrationv1alpha1.GroupVersion})
	mapper.Add(orchestrationv1alpha1.GroupVersion.WithKind(StormService), apimeta.RESTScopeNamespace)

	base := mustParseTime(t, "2026-08-03T09:30:00Z")
	now := base
	autoScaler := &fakeAutoScaler{}
	r := &PodAutoscalerReconciler{
		Client:              cl,
		Scheme:              sch,
		EventRecorder:       record.NewFakeRecorder(100),
		Mapper:              mapper,
		workloadScaleClient: NewWorkloadScale(cl, nil),
		autoScaler:          autoScaler,
		recommendations:     make(map[string][]timestampedRecommendation),
		monitor:             monitor.New(),
		scalingTargetToPA:   make(map[string]ktypes.NamespacedName),
		paToScalingKey:      make(map[ktypes.NamespacedName]string),
		now:                 func() time.Time { return now },
	}
	templateReplicas := func() int32 {
		t.Helper()
		got := &orchestrationv1alpha1.StormService{}
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(ss), got))
		return *got.Spec.Template.Spec.Roles[0].Replicas
	}

	// The pass that scales up: the template goes from 2 to the recommended 6.
	autoScaler.result = &ReplicaComputeResult{DesiredReplicas: 6, Valid: true}
	_, err := r.scaleCustomPA(ctx, pa)
	require.NoError(t, err)
	require.Equal(t, int32(6), templateReplicas())

	// Fifteen seconds later the 4 new pods do not exist yet, and the recommendation dips.
	now = base.Add(15 * time.Second)
	autoScaler.result = &ReplicaComputeResult{DesiredReplicas: 3, Valid: true}
	_, err = r.scaleCustomPA(ctx, pa)
	require.NoError(t, err)

	assert.Equal(t, int32(6), autoScaler.lastRequest.CurrentReplicas,
		"the decision starts from the template count the first pass wrote")
	assert.Equal(t, int32(6), templateReplicas(), "the scale-down window holds the role at 6")
	assert.Equal(t, int32(2), pa.Status.ActualScale, "actualScale reports the pods that exist")
	assert.Equal(t, int32(6), pa.Status.DesiredScale)
	scalingActive := apimeta.FindStatusCondition(pa.Status.Conditions, ConditionScalingActive)
	require.NotNil(t, scalingActive)
	assert.Equal(t, metav1.ConditionTrue, scalingActive.Status, "the role has not reached its target yet")
}
