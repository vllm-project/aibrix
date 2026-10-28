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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/monitor"
)

func readyRoleSetPod(name, roleSet, role string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{
			constants.StormServiceNameLabelKey: "ss-replica",
			constants.RoleSetNameLabelKey:      roleSet,
			constants.RoleNameLabelKey:         role,
		}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "10.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// scaleReplicaModeStormService runs one scaling pass of the controller against a
// replica-mode StormService whose RoleSets each run a prefill and a decode pod, and
// returns the replicas it writes. Without a subTargetSelector both pods of every
// RoleSet report the metric, but the controller scales RoleSets. RoleSets past
// readyRoleSets are still starting: their pods are not ready and not serving.
func scaleReplicaModeStormService(t *testing.T, strategy autoscalingv1alpha1.ScalingStrategyType, replicas, readyRoleSets int, perPod float64) int32 {
	t.Helper()
	ctx := context.Background()
	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)
	_ = orchestrationv1alpha1.AddToScheme(sch)

	ss := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ss-replica"},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Mode:     orchestrationv1alpha1.StormServiceReplicaMode,
			Replicas: ptr.To(int32(replicas)),
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "prefill", Replicas: ptr.To(int32(1))},
						{Name: "decode", Replicas: ptr.To(int32(1))},
					},
				},
			},
		},
	}
	objs := []client.Object{ss}
	unscrapable := map[string]bool{}
	for i := 0; i < replicas; i++ {
		roleSet := fmt.Sprintf("ss-replica-roleset-%d", i)
		for _, role := range []string{"prefill", "decode"} {
			pod := readyRoleSetPod(fmt.Sprintf("%s-%d", role, i), roleSet, role)
			if i >= readyRoleSets {
				pod.Status.Conditions = nil
				unscrapable[pod.Name] = true
			}
			objs = append(objs, pod)
		}
	}
	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ss-pa"},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: orchestrationv1alpha1.GroupVersion.String(),
				Kind:       StormService,
				Name:       ss.Name,
			},
			MinReplicas:     ptr.To(int32(1)),
			MaxReplicas:     20,
			ScalingStrategy: strategy,
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.POD,
				ProtocolType:     autoscalingv1alpha1.HTTP,
				Port:             "8000",
				Path:             "/metrics",
				TargetMetric:     "num_requests_running",
				TargetValue:      "50",
			}},
		},
	}
	objs = append(objs, pa)
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{orchestrationv1alpha1.GroupVersion})
	mapper.Add(orchestrationv1alpha1.GroupVersion.WithKind(StormService), apimeta.RESTScopeNamespace)

	now := mustParseTime(t, "2026-10-28T09:00:00Z")
	fetcher := &mockMetricFetcherFactory{mockMetricFetcher{metricsValue: perPod, unscrapable: unscrapable}}
	r := &PodAutoscalerReconciler{
		Client:              cl,
		Scheme:              sch,
		EventRecorder:       record.NewFakeRecorder(100),
		Mapper:              mapper,
		workloadScaleClient: NewWorkloadScale(cl, nil),
		autoScaler:          NewDefaultAutoScaler(fetcher, cl),
		recommendations:     make(map[string][]timestampedRecommendation),
		monitor:             monitor.New(),
		scalingTargetToPA:   make(map[string]ktypes.NamespacedName),
		paToScalingKey:      make(map[ktypes.NamespacedName]string),
		now:                 func() time.Time { return now },
	}

	_, err := r.scaleCustomPA(ctx, pa)
	require.NoError(t, err)

	got := &orchestrationv1alpha1.StormService{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(ss), got))
	return *got.Spec.Replicas
}

// TestKPAReplicaModeStormServiceSizesRoleSets checks that KPA counts the 3 RoleSets
// carrying the load, not their 6 pods.
func TestKPAReplicaModeStormServiceSizesRoleSets(t *testing.T) {
	assert.Equal(t, int32(3), scaleReplicaModeStormService(t, autoscalingv1alpha1.KPA, 3, 3, 40), "every pod below target")
	assert.Equal(t, int32(4), scaleReplicaModeStormService(t, autoscalingv1alpha1.KPA, 3, 3, 60), "every pod 20% above target")
}

// TestReplicaModeStormServiceHoldsForAStartingRoleSet checks that a RoleSet whose pods
// are not ready yet counts as a pending replica, as a pending Deployment pod does, so
// the pending-replica guard holds the scale-up it has already asked for.
func TestReplicaModeStormServiceHoldsForAStartingRoleSet(t *testing.T) {
	for _, strategy := range []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.APA, autoscalingv1alpha1.KPA} {
		t.Run(string(strategy), func(t *testing.T) {
			// 3 RoleSets serve at 60 per pod against a target of 50 and a 4th is starting:
			// 3.6 RoleSets of load, so the 4 already asked for are enough.
			assert.Equal(t, int32(4), scaleReplicaModeStormService(t, strategy, 4, 3, 60))
		})
	}
}

// TestDeploymentHoldsForAStartingPod is the Deployment case the RoleSet test above
// matches: 3 ready pods at 60 against a target of 50 and a 4th pod starting.
func TestDeploymentHoldsForAStartingPod(t *testing.T) {
	for _, strategy := range []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.APA, autoscalingv1alpha1.KPA} {
		t.Run(string(strategy), func(t *testing.T) {
			ctx := context.Background()
			sch := runtime.NewScheme()
			_ = scheme.AddToScheme(sch)
			_ = autoscalingv1alpha1.AddToScheme(sch)
			lbls := map[string]string{"app": "llm"}
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "llm"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(4)), Selector: &metav1.LabelSelector{MatchLabels: lbls}},
			}
			pa := &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "llm-pa"},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef:  corev1.ObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name},
					MinReplicas:     ptr.To(int32(1)),
					MaxReplicas:     20,
					ScalingStrategy: strategy,
					MetricsSources: []autoscalingv1alpha1.MetricSource{{
						MetricSourceType: autoscalingv1alpha1.POD,
						ProtocolType:     autoscalingv1alpha1.HTTP,
						Port:             "8000",
						Path:             "/metrics",
						TargetMetric:     "num_requests_running",
						TargetValue:      "50",
					}},
				},
			}
			cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(dep, pa,
				readyPod(ns, "llm-0", lbls), readyPod(ns, "llm-1", lbls), readyPod(ns, "llm-2", lbls), notReadyPod(ns, "llm-3", lbls)).Build()
			mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{appsv1.SchemeGroupVersion})
			mapper.Add(appsv1.SchemeGroupVersion.WithKind("Deployment"), apimeta.RESTScopeNamespace)

			now := mustParseTime(t, "2026-10-28T09:00:00Z")
			fetcher := &mockMetricFetcherFactory{mockMetricFetcher{metricsValue: 60, unscrapable: map[string]bool{"llm-3": true}}}
			r := &PodAutoscalerReconciler{
				Client:              cl,
				Scheme:              sch,
				EventRecorder:       record.NewFakeRecorder(100),
				Mapper:              mapper,
				workloadScaleClient: NewWorkloadScale(cl, nil),
				autoScaler:          NewDefaultAutoScaler(fetcher, cl),
				recommendations:     make(map[string][]timestampedRecommendation),
				monitor:             monitor.New(),
				scalingTargetToPA:   make(map[string]ktypes.NamespacedName),
				paToScalingKey:      make(map[ktypes.NamespacedName]string),
				now:                 func() time.Time { return now },
			}

			_, err := r.scaleCustomPA(ctx, pa)
			require.NoError(t, err)

			got := &appsv1.Deployment{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(dep), got))
			assert.Equal(t, int32(4), *got.Spec.Replicas)
		})
	}
}

func TestPodsPerReplica(t *testing.T) {
	stormService := func(mode orchestrationv1alpha1.StormServiceMode) map[string]interface{} {
		return map[string]interface{}{
			"apiVersion": orchestrationv1alpha1.GroupVersion.String(),
			"kind":       StormService,
			"metadata":   map[string]interface{}{"name": "ss-replica", "namespace": ns},
			"spec":       map[string]interface{}{"mode": string(mode), "replicas": int64(3)},
		}
	}
	roleSetPods := func(roleSets, perRoleSet int) []corev1.Pod {
		var pods []corev1.Pod
		for i := 0; i < roleSets; i++ {
			for j := 0; j < perRoleSet; j++ {
				pods = append(pods, *readyRoleSetPod(fmt.Sprintf("pod-%d-%d", i, j), fmt.Sprintf("roleset-%d", i), "decode"))
			}
		}
		return pods
	}
	terminating := roleSetPods(1, 1)[0]
	terminating.Name = "terminating"
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}

	for _, tc := range []struct {
		name     string
		scale    *unstructured.Unstructured
		roleName string
		pods     []corev1.Pod
		want     float64
	}{
		{
			name:  "deployment",
			scale: buildScaleObject("apps/v1", "Deployment", ns, "llm"),
			pods:  roleSetPods(3, 2),
			want:  1,
		},
		{
			name:  "stormservice without a subTargetSelector scales RoleSets",
			scale: &unstructured.Unstructured{Object: stormService(orchestrationv1alpha1.StormServicePooledMode)},
			pods:  roleSetPods(3, 2),
			want:  2,
		},
		{
			name:     "replica-mode stormservice with a subTargetSelector scales RoleSets",
			scale:    &unstructured.Unstructured{Object: stormService(orchestrationv1alpha1.StormServiceReplicaMode)},
			roleName: "decode",
			pods:     roleSetPods(3, 4),
			want:     4,
		},
		{
			name:     "pooled-mode stormservice with a subTargetSelector scales pods",
			scale:    &unstructured.Unstructured{Object: stormService(orchestrationv1alpha1.StormServicePooledMode)},
			roleName: "decode",
			pods:     roleSetPods(1, 4),
			want:     1,
		},
		{
			name:  "terminating pods are not counted",
			scale: &unstructured.Unstructured{Object: stormService(orchestrationv1alpha1.StormServiceReplicaMode)},
			pods:  append(roleSetPods(2, 2), terminating),
			want:  2,
		},
		{
			name:  "pods without a RoleSet label fall back to one",
			scale: &unstructured.Unstructured{Object: stormService(orchestrationv1alpha1.StormServiceReplicaMode)},
			pods:  []corev1.Pod{*buildPod(ns, "p-0", nil)},
			want:  1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pa := &autoscalingv1alpha1.PodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pa"}}
			if tc.roleName != "" {
				pa.Spec.SubTargetSelector = &autoscalingv1alpha1.SubTargetSelector{RoleName: tc.roleName}
			}
			roleSets, err := scalesRoleSets(pa, tc.scale)
			require.NoError(t, err)
			assert.Equal(t, tc.want, podsPerReplica(roleSets, tc.pods))
		})
	}
}

func TestReplicaStateFromPodsCountsRoleSets(t *testing.T) {
	starting := func(name, roleSet string) corev1.Pod {
		pod := readyRoleSetPod(name, roleSet, "decode")
		pod.Status.Conditions = nil
		return *pod
	}
	pods := []corev1.Pod{
		*readyRoleSetPod("p-0", "rs-0", "prefill"), *readyRoleSetPod("d-0", "rs-0", "decode"),
		*readyRoleSetPod("p-1", "rs-1", "prefill"), *readyRoleSetPod("d-1", "rs-1", "decode"),
		*readyRoleSetPod("p-2", "rs-2", "prefill"), starting("d-2", "rs-2"),
		starting("p-3", "rs-3"), starting("d-3", "rs-3"),
	}

	// RoleSets: rs-0 and rs-1 are ready; rs-2 waits on its decode pod, rs-3 on both.
	assert.Equal(t, ReplicaState{ReadyReplicas: 2, PendingReplicas: 2}, replicaStateFromPods(pods, 4, true))
	// A fifth RoleSet with no pods yet is pending too.
	assert.Equal(t, ReplicaState{ReadyReplicas: 2, PendingReplicas: 3}, replicaStateFromPods(pods, 5, true))
	// Counted as pods, the 5 ready pods cover all 4 replicas.
	assert.Equal(t, ReplicaState{ReadyReplicas: 4, PendingReplicas: 0}, replicaStateFromPods(pods, 4, false))
}
