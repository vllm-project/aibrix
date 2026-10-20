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

package prefixcache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
)

// matchedPods returns the pods the router's index holds the prompt on.
func matchedPods(t *testing.T, r *prefixCacheRouter, pods types.PodList, model, prompt string) map[string]int {
	t.Helper()
	ready := map[string]struct{}{}
	for _, pod := range pods.All() {
		ready[utils.GeneratePodKey(pod.Namespace, pod.Name)] = struct{}{}
	}
	if r.kvSyncRouter != nil {
		matched, _ := r.kvSyncRouter.syncIndexer.MatchPrefix(model, -1, []byte(prompt), ready)
		return matched
	}
	matched, _ := r.prefixCacheIndexer.MatchPrefix([]byte(prompt), model, ready)
	return matched
}

// A request its target pod has no room for is rejected by the gateway's requestsInflight
// cap. Indexing its prompt on that pod first left an entry for a prompt the pod never
// received, and the next requests with the same prefix were routed to it for a cache hit
// that is not there. The router has to admit its target before it indexes the prompt.
func TestPrefixCacheRouteIndexesOnlyAdmittedTarget(t *testing.T) {
	const prompt = "a prompt long enough to fill several prefix blocks"
	for _, kvSync := range []bool{false, true} {
		mode := "local-index"
		if kvSync {
			mode = "kv-sync"
		}
		t.Run(mode+"/rejected", func(t *testing.T) {
			r, pods := newPrefixContextRouter(t, tokenizer.NewCharacterTokenizer(), kvSync)
			ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "test-model", prompt, "rejected", "")
			t.Cleanup(ctx.Delete)
			var admitted []string
			ctx.SetTargetAdmitter(func(pod *v1.Pod) error {
				admitted = append(admitted, pod.Name)
				return types.ErrTargetReplicaInflightExceeded
			})

			_, err := r.Route(ctx, pods)
			require.ErrorIs(t, err, types.ErrTargetReplicaInflightExceeded)
			assert.Len(t, admitted, 1)
			assert.Empty(t, matchedPods(t, r, pods, "test-model", prompt), "a rejected request must not be indexed on any pod")
		})
		t.Run(mode+"/admitted", func(t *testing.T) {
			r, pods := newPrefixContextRouter(t, tokenizer.NewCharacterTokenizer(), kvSync)
			ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "test-model", prompt, "admitted", "")
			t.Cleanup(ctx.Delete)
			ctx.SetTargetAdmitter(func(*v1.Pod) error { return nil })

			_, err := r.Route(ctx, pods)
			require.NoError(t, err)
			target := utils.GeneratePodKey(ctx.TargetPod().Namespace, ctx.TargetPod().Name)
			assert.Equal(t, 100, matchedPods(t, r, pods, "test-model", prompt)[target])
		})
	}
}

// prefix-cache-preble records the prompt and the request's decode load on its target in
// PostRouteUpdate, from inside Route, so it admits the target first as well.
func TestPrebleRouteRecordsOnlyAdmittedTarget(t *testing.T) {
	r := newTestRouter(10, nil)
	pods := &MockPodList{pods: []*v1.Pod{
		newPod("preble-pod-1", "10.0.0.1", true, nil),
		newPod("preble-pod-2", "10.0.0.2", true, nil),
	}}
	ctx := createTestRoutingContext("test-model", "a prompt for preble", "rejected")
	t.Cleanup(ctx.Delete)
	ctx.SetTargetAdmitter(func(*v1.Pod) error { return types.ErrTargetReplicaInflightExceeded })

	_, err := r.Route(ctx, pods)
	require.ErrorIs(t, err, types.ErrTargetReplicaInflightExceeded)
	assert.Empty(t, r.histogram.timestamps, "a rejected request must not be recorded on any pod")
}
