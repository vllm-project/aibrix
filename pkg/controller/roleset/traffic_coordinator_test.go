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
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/trafficdrain"
)

func TestTrafficDrainLostPublishedStateNeverBecomesIdle(t *testing.T) {
	var body atomic.Value
	body.Store(trafficGauges("0", "0"))
	pod := trafficTestPod(t, "lost", &body, 200)
	trafficMarkDraining(pod, 2*time.Second)
	syncer, cli, owner, role, mr := trafficFixtureBackend(t, pod)
	epoch := pod.Annotations[constants.TrafficDrainEpochAnnotationKey]
	mr.FlushAll()
	_, err := syncer.Scale(context.Background(), owner, role)
	require.Error(t, err)
	got := &v1.Pod{}
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	require.Equal(t, epoch, got.Annotations[constants.TrafficDrainEpochAnnotationKey])
	require.Empty(t, mr.Keys(), "a published identity must not be silently reinitialized")
}

func TestTrafficDrainCorruptCounterNeverAuthorizesDeletion(t *testing.T) {
	var body atomic.Value
	body.Store(trafficGauges("0", "0"))
	pod := trafficTestPod(t, "corrupt", &body, 200)
	syncer, cli, owner, role, mr := trafficFixtureBackend(t, pod)
	epoch := pod.Annotations[constants.TrafficDrainEpochAnnotationKey]
	require.NoError(t, syncer.gate.Admit(context.Background(), string(pod.UID), epoch, "held"))
	mr.HSet("aibrix:traffic-drain:{"+string(pod.UID)+"}", "active", "0")
	_, err := syncer.Scale(context.Background(), owner, role)
	require.Error(t, err)
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
}

func TestTrafficDrainRestartFinishesSealedDeletionBeforeReplacing(t *testing.T) {
	ctx := context.Background()
	var body atomic.Value
	body.Store(trafficGauges("0", "0"))
	pod := trafficTestPod(t, "sealed", &body, 200)
	trafficMarkDraining(pod, 2*time.Second)
	syncer, cli, owner, role, mr := trafficFixtureBackend(t, pod)
	epoch := pod.Annotations[constants.TrafficDrainEpochAnnotationKey]
	state, err := syncer.gate.Read(ctx, string(pod.UID), epoch)
	require.NoError(t, err)
	_, err = syncer.gate.Seal(ctx, string(pod.UID), epoch, state.Generation)
	require.NoError(t, err)
	role.Replicas = ptr.To[int32](1)
	connection := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = connection.Close() }()
	newGate := trafficdrain.New(connection)
	defer func() { _ = newGate.Close() }()
	restarted := &StatelessRoleSyncer{cli: cli, gate: newGate, computeHashFunc: fakeComputeHashFunc}
	_, err = restarted.Scale(ctx, owner, role)
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(cli.Get(ctx, client.ObjectKeyFromObject(pod), &v1.Pod{})), "sealed admission cannot reopen even if desired capacity increases")
	_, err = restarted.Scale(ctx, owner, role)
	require.NoError(t, err)
	list := &v1.PodList{}
	require.NoError(t, cli.List(ctx, list))
	require.Len(t, list.Items, 1)
	created := &list.Items[0]
	require.Equal(t, constants.TrafficDrainProtocolVersion, created.Annotations[constants.TrafficDrainProtocolAnnotationKey])
	require.Empty(t, created.Annotations[constants.TrafficDrainEpochAnnotationKey])
	// The fake API does not allocate UIDs. Supply the server-assigned identity
	// before the next reconciliation, as a real Pod creation would do.
	created.UID = "replacement-uid"
	require.NoError(t, cli.Update(ctx, created))
	_, err = restarted.Scale(ctx, owner, role)
	require.NoError(t, err)
	ready := &v1.Pod{}
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(created), ready))
	newEpoch := ready.Annotations[constants.TrafficDrainEpochAnnotationKey]
	require.NotEmpty(t, newEpoch)
	state, err = newGate.Read(ctx, string(ready.UID), newEpoch)
	require.NoError(t, err)
	require.Equal(t, trafficdrain.Accepting, state.Phase)
}

func TestTrafficDrainCannotBypassProtectionByChangingRoleType(t *testing.T) {
	var body atomic.Value
	body.Store(trafficGauges("0", "0"))
	pod := trafficTestPod(t, "protected", &body, 200)
	_, cli, owner, role := trafficFixture(t, pod)
	delete(role.Template.Annotations, trafficWaitKey)
	role.Stateful = true
	owner.Spec.Roles = []orchestrationv1alpha1.RoleSpec{*role}
	r := &RoleSetReconciler{Client: cli}
	_, err := r.syncPods(context.Background(), owner)
	require.Error(t, err)
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
}
