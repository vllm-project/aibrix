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

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	clientv3 "go.etcd.io/etcd/client/v3"
	v1 "k8s.io/api/core/v1"
)

// TestEtcdCompactionRecoveryIntegration requires a dedicated disposable etcd.
// Compaction affects server-wide history, even though this test writes and deletes
// only keys within its own UUID prefix. Never point the environment variable at
// a production or shared server, and do not run this test in parallel with other
// tests that depend on historical revisions.
func TestEtcdCompactionRecoveryIntegration(t *testing.T) {
	endpoint := os.Getenv("AIBRIX_TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("set AIBRIX_TEST_ETCD_ENDPOINT to a dedicated disposable etcd; this test compacts server-wide history")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	require.NoError(t, err)
	prefix := "/aibrix/discovery-integration/" + uuid.NewString() + "/"
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	var wrapped *compactingEtcdClient
	t.Cleanup(func() {
		stopOnce.Do(func() { close(stopCh) })
		if wrapped != nil {
			select {
			case <-wrapped.closed:
			case <-time.After(5 * time.Second):
				t.Error("provider did not close its etcd client after stop")
			}
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, err := admin.Delete(cleanupCtx, prefix, clientv3.WithPrefix())
		assert.NoError(t, err)
		assert.NoError(t, admin.Close())
	})
	value := func(model, engine string) string {
		data, err := json.Marshal(EtcdEndpoint{Model: model, Address: model + ":8000", Engine: engine})
		require.NoError(t, err)
		return string(data)
	}
	oldValue, stableValue, newValue := value("old", "vllm"), value("stable", "vllm"), value("new", "vllm")
	_, err = admin.Txn(ctx).Then(clientv3.OpPut(prefix+"old", oldValue), clientv3.OpPut(prefix+"stable", stableValue)).Commit()
	require.NoError(t, err)

	provider, err := NewEtcdProvider(EtcdConfig{Endpoints: []string{endpoint}, Prefix: prefix})
	require.NoError(t, err)
	provider.newClient = func(config clientv3.Config) (etcdClient, error) {
		client, err := clientv3.New(config)
		if err != nil {
			return nil, err
		}
		wrapped = &compactingEtcdClient{
			etcdClient: client, compacted: make(chan int64, 1), closed: make(chan struct{}),
			afterFirstSnapshot: func(requestCtx context.Context, snapshot *clientv3.GetResponse) error {
				_, err := admin.Txn(requestCtx).Then(clientv3.OpDelete(prefix+"old"), clientv3.OpPut(prefix+"new", newValue)).Commit()
				if err != nil {
					return err
				}
				// Advance beyond snapshot+1 without changing this surviving route.
				barrier, err := admin.Put(requestCtx, prefix+"stable", stableValue)
				if err != nil {
					return err
				}
				if barrier.Header.Revision <= snapshot.Header.Revision+1 {
					return fmt.Errorf("compaction barrier did not advance beyond watch start revision")
				}
				_, err = admin.Compact(requestCtx, barrier.Header.Revision, clientv3.WithCompactPhysical())
				return err
			},
		}
		return wrapped, nil
	}

	var mu sync.Mutex
	state := make(map[string]*v1.Pod)
	var events []WatchEvent
	err = provider.Watch(func(event WatchEvent) {
		mu.Lock()
		defer mu.Unlock()
		pod := event.Object.(*v1.Pod)
		model := pod.Labels[constants.ModelLabelName]
		if event.Type == EventDelete {
			delete(state, model)
		} else {
			state[model] = pod
		}
		events = append(events, event)
	}, stopCh)
	require.NoError(t, err)
	select {
	case revision := <-wrapped.compacted:
		t.Logf("real etcd watch rejected compacted revision; CompactRevision=%d", revision)
	case <-ctx.Done():
		t.Fatal("did not observe a real compacted watch response")
	}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(state) == 2 && state["old"] == nil && state["new"] != nil && state["stable"] != nil
	}, 10*time.Second, 10*time.Millisecond, "resnapshot must remove stale registration and discover replacement")

	// A transaction barrier proves the recovered watch keeps processing events.
	_, err = admin.Txn(ctx).Then(clientv3.OpPut(prefix+"new", value("new", "sglang")), clientv3.OpPut(prefix+"barrier", value("barrier", "vllm"))).Commit()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return state["barrier"] != nil && state["new"].Labels[constants.ModelLabelEngine] == "sglang"
	}, 10*time.Second, 10*time.Millisecond, "watch must continue after compaction recovery")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 6)
	counts := make(map[string]map[EventType]int)
	for _, event := range events {
		model := event.Object.(*v1.Pod).Labels[constants.ModelLabelName]
		if counts[model] == nil {
			counts[model] = make(map[EventType]int)
		}
		counts[model][event.Type]++
	}
	assert.Equal(t, map[EventType]int{EventAdd: 1, EventDelete: 1}, counts["old"])
	assert.Equal(t, map[EventType]int{EventAdd: 1}, counts["stable"], "unchanged route must not be re-added or updated")
	assert.Equal(t, map[EventType]int{EventAdd: 1, EventUpdate: 1}, counts["new"])
	assert.Equal(t, map[EventType]int{EventAdd: 1}, counts["barrier"])
}

// compactingEtcdClient keeps all reads and watches on real etcd. Its one-time
// hook forces history to be compacted between the provider's first Get and Watch;
// forwarding the real watch response also verifies compaction actually occurred.
type compactingEtcdClient struct {
	etcdClient
	afterFirstSnapshot func(context.Context, *clientv3.GetResponse) error
	once               sync.Once
	compacted          chan int64
	closed             chan struct{}
}

func (c *compactingEtcdClient) Get(ctx context.Context, key string, options ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	response, err := c.etcdClient.Get(ctx, key, options...)
	if err != nil {
		return nil, err
	}
	c.once.Do(func() { err = c.afterFirstSnapshot(ctx, response) })
	return response, err
}

func (c *compactingEtcdClient) Watch(ctx context.Context, key string, options ...clientv3.OpOption) clientv3.WatchChan {
	stream := c.etcdClient.Watch(ctx, key, options...)
	forwarded := make(chan clientv3.WatchResponse)
	go func() {
		defer close(forwarded)
		for {
			select {
			case <-ctx.Done():
				return
			case response, ok := <-stream:
				if !ok {
					return
				}
				if response.CompactRevision > 0 {
					select {
					case c.compacted <- response.CompactRevision:
					default:
					}
				}
				select {
				case forwarded <- response:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return forwarded
}

func (c *compactingEtcdClient) Close() error {
	err := c.etcdClient.Close()
	close(c.closed)
	return err
}
