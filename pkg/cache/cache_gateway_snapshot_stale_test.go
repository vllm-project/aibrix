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

package cache

import (
	"context"
	"fmt"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/utils"
)

// These tests pin how old the other gateways' counts that GetPodsRunningRequestsApprox adds
// may be. remoteRunningMaxAge promises that a count older than it is dropped, so the candidate
// stage falls back to this gateway's own counts. A tick that cannot read some keys carries
// their last known values over, but publishes the result with that tick's start time, so a
// count that has not been read for any length of time still looks fresh.

// addTestMetaPod adds a pod to store.metaPods and removes it again when the test ends. The sync
// reads gatewayPodName only to write this gateway's snapshot of each pod in metaPods, and it
// sees its stop channel only between ticks, so it may run a tick or two after the test; without
// the pod those ticks no longer touch the variable that later tests reassign.
func addTestMetaPod(t *testing.T, store *Store, name string) *v1.Pod {
	t.Helper()
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	key := utils.GeneratePodKey(testNamespace, name)
	store.metaPods.Store(key, &Pod{Pod: pod})
	t.Cleanup(func() { store.metaPods.Delete(key) })
	return pod
}

func approxRunning(store *Store, pod *v1.Pod) int64 {
	return store.GetPodsRunningRequestsApprox([]*v1.Pod{pod})[utils.GeneratePodKey(pod.Namespace, pod.Name)]
}

// Every read fails from some point on. The remote count stops being refreshed, so once it is
// older than remoteRunningMaxAge it must no longer be added.
func TestRemoteRunningApprox_DropsCountsNotReadWithinMaxAge(t *testing.T) {
	store, client, hook, remoteKey := newSnapshotSyncUnderTest(t)
	pod := addTestMetaPod(t, store, testPodName)
	require.Eventually(t, func() bool { return approxRunning(store, pod) == 2 },
		2*time.Second, 20*time.Millisecond, "the remote count must be read before the failure")

	hook.failReads.Store(true)
	// The remote gateway's requests finish while this gateway cannot read them.
	setRemoteRunning(t, client, remoteKey, "0")

	time.Sleep(remoteRunningMaxAge + 5*gatewaySnapshotSyncInterval)
	require.Equal(t, int64(0), approxRunning(store, pod),
		"a remote count last read more than remoteRunningMaxAge ago must not be added")
}

// tailTimeoutHook makes the HGETALL batch holding tailKey run into the tick's deadline, as a
// tick that has more keys to read than its 100 ms allow does: the batches before it succeed and
// the rest fail with context.DeadlineExceeded. It is active between slow and release being
// closed. Closing release also ends the wait of a batch in flight, so ticks the sync still runs
// after the test are as quick as in any other test (it sees its stop channel only between
// ticks) instead of each lasting until its deadline.
type tailTimeoutHook struct {
	tailKey string
	slow    chan struct{}
	release chan struct{}
}

func (h *tailTimeoutHook) active() bool {
	select {
	case <-h.release:
		return false
	default:
	}
	select {
	case <-h.slow:
		return true
	default:
		return false
	}
}

func (h *tailTimeoutHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *tailTimeoutHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (h *tailTimeoutHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.active() {
			for _, cmd := range cmds {
				if cmd.Name() == "hgetall" && len(cmd.Args()) > 1 && cmd.Args()[1] == h.tailKey {
					select {
					case <-ctx.Done():
					case <-h.release:
						return next(ctx, cmds)
					}
					for _, c := range cmds {
						c.SetErr(ctx.Err())
					}
					return ctx.Err()
				}
			}
		}
		return next(ctx, cmds)
	}
}

// The realistic case: one batch of keys keeps running out of time on every tick while the
// batches before it are read. The pods in the timed-out batch keep the counts they had when the
// deadline was first missed, for as long as the tick stays too slow.
func TestRemoteRunningApprox_DropsCountsOfBatchThatKeepsTimingOut(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	// gatewayPodName is left as it is: the remote keys belong to "gw-remote", never to this
	// gateway, and the sync reads gatewayPodName on every tick, which this test keeps running
	// until its deadline.

	// More remote keys than one HGETALL batch, so the first and the last pod are read in
	// different batches.
	n := gatewaySnapshotHGetAllBatchSize + 50
	podName := func(i int) string { return fmt.Sprintf("pod-%03d", i) }
	setRemote := func(i int, running string) {
		require.NoError(t, client.HSet(context.Background(), gatewayPodSnapshotKey("gw-remote", testNamespace, podName(i)),
			map[string]any{
				"gateway_instance_id": "gw-remote",
				"namespace":           testNamespace,
				"pod_name":            podName(i),
				"requests_running":    running,
			}).Err())
	}
	for i := range n {
		setRemote(i, "3")
	}

	hook := &tailTimeoutHook{
		tailKey: gatewayPodSnapshotKey("gw-remote", testNamespace, podName(n-1)),
		slow:    make(chan struct{}),
		release: make(chan struct{}),
	}
	client.AddHook(hook)

	store := &Store{redisClient: client}
	stopCh := make(chan struct{})
	t.Cleanup(func() { close(hook.release); close(stopCh) })
	head, tail := addTestMetaPod(t, store, podName(0)), addTestMetaPod(t, store, podName(n-1))
	initGatewaySnapshotSync(store, stopCh)

	require.Eventually(t, func() bool { return approxRunning(store, head) == 3 && approxRunning(store, tail) == 3 },
		2*time.Second, 20*time.Millisecond, "every remote count must be read before the slowdown")

	close(hook.slow)
	for i := range n {
		setRemote(i, "0")
	}

	require.Eventually(t, func() bool { return approxRunning(store, head) == 0 },
		2*time.Second, 20*time.Millisecond, "the batches that still fit in the tick keep being refreshed")
	time.Sleep(remoteRunningMaxAge + 5*gatewaySnapshotSyncInterval)
	require.Equal(t, int64(0), approxRunning(store, tail),
		"a pod whose batch has not been read for more than remoteRunningMaxAge must not keep its old remote count")
}
