/*
Copyright 2024 The Aibrix Team.

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
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	podutil "github.com/vllm-project/aibrix/pkg/utils"
)

// shuffledListClient returns pods in a different order on every List, the
// way a List from the informer cache can.
type shuffledListClient struct {
	client.Client
	rng *rand.Rand
}

func (c *shuffledListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if pods, ok := list.(*v1.PodList); ok {
		c.rng.Shuffle(len(pods.Items), func(i, j int) {
			pods.Items[i], pods.Items[j] = pods.Items[j], pods.Items[i]
		})
	}
	return nil
}

// newDrainRolloutFixture returns a syncer over replicas ready outdated pods
// that were created in the same second, for a role with drain configured.
func newDrainRolloutFixture(t *testing.T, replicas int32, maxUnavailable int, seed int64) (*StatelessRoleSyncer, client.Client, *orchestrationv1alpha1.RoleSet, *orchestrationv1alpha1.RoleSpec) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", replicas, intStrPtr(intstr.FromInt(0)), intStrPtr(intstr.FromInt(maxUnavailable)))
	timeout := int32(60)
	role.Drain = &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout}

	var objs []client.Object
	for i := int32(0); i < replicas; i++ {
		objs = append(objs, newTestPodWithHash(fmt.Sprintf("pod-%d", i), "test-ns", true, false, oldHash))
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	syncer := &StatelessRoleSyncer{
		cli:             &shuffledListClient{Client: fakeClient, rng: rand.New(rand.NewSource(seed))},
		computeHashFunc: fakeComputeHashFunc,
		recorder:        record.NewFakeRecorder(1000),
	}
	return syncer, fakeClient, roleSet, role
}

func countDrainingPods(t *testing.T, c client.Client) int {
	t.Helper()
	pods := &v1.PodList{}
	require.NoError(t, c.List(context.Background(), pods))
	draining := 0
	for i := range pods.Items {
		if podutil.IsPodDraining(&pods.Items[i]) {
			draining++
		}
	}
	return draining
}

// TestStatelessRolloutDrainsAtMostMaxUnavailable covers #3571: a draining pod
// stays Ready, so before the fix each pass had a budget of maxUnavailable
// again, and picked a pod from a list whose order changes between passes.
func TestStatelessRolloutDrainsAtMostMaxUnavailable(t *testing.T) {
	for _, maxUnavailable := range []int{1, 2} {
		for seed := int64(1); seed <= 5; seed++ {
			t.Run(fmt.Sprintf("maxUnavailable=%d/seed=%d", maxUnavailable, seed), func(t *testing.T) {
				syncer, fakeClient, roleSet, role := newDrainRolloutFixture(t, 4, maxUnavailable, seed)
				for pass := 1; pass <= 10; pass++ {
					_, err := syncer.Rollout(context.Background(), roleSet, role)
					require.NoError(t, err)
					assert.LessOrEqual(t, countDrainingPods(t, fakeClient), maxUnavailable, "pass %d", pass)
				}
				assert.Equal(t, maxUnavailable, countDrainingPods(t, fakeClient))
			})
		}
	}
}

func TestStatelessRolloutByStepDrainsAtMostMaxUnavailable(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			syncer, fakeClient, roleSet, role := newDrainRolloutFixture(t, 4, 1, seed)
			for pass := 1; pass <= 10; pass++ {
				_, err := syncer.RolloutByStep(context.Background(), roleSet, role, 1)
				require.NoError(t, err)
				assert.LessOrEqual(t, countDrainingPods(t, fakeClient), 1, "pass %d", pass)
			}
			assert.Equal(t, 1, countDrainingPods(t, fakeClient))
		})
	}
}

// TestStatelessRolloutDeletesExpiredDrainBeforeStartingAnother checks that the
// rollout still progresses: the pass that finds a drain expired deletes that
// pod, and doesn't start draining another one until the budget allows it.
func TestStatelessRolloutDeletesExpiredDrainBeforeStartingAnother(t *testing.T) {
	syncer, fakeClient, roleSet, role := newDrainRolloutFixture(t, 4, 1, 1)
	drained := &v1.Pod{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "test-ns", Name: "pod-2"}, drained))
	drained.Annotations = map[string]string{
		aibrixconst.PodDrainingAnnotationKey:          "true",
		aibrixconst.PodDrainStartTimeAnnotationKey:    time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
		aibrixconst.PodDrainReasonAnnotationKey:       aibrixconst.PodDrainReasonRollout,
		aibrixconst.PodDrainTargetActionAnnotationKey: aibrixconst.PodDrainTargetActionDelete,
	}
	require.NoError(t, fakeClient.Update(context.Background(), drained))

	_, err := syncer.Rollout(context.Background(), roleSet, role)
	require.NoError(t, err)

	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "test-ns", Name: "pod-2"}, &v1.Pod{})
	assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "the pod whose drain expired should be deleted, got err=%v", err)
	assert.Zero(t, countDrainingPods(t, fakeClient), "no new drain should start while the deleted pod's replacement isn't there yet")
}
