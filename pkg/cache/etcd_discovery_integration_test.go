// Copyright 2026 AIBrix Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cache

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	"github.com/vllm-project/aibrix/pkg/constants"
	clientv3 "go.etcd.io/etcd/client/v3"
	v1 "k8s.io/api/core/v1"
)

const etcdDiscoveryTestTimeout = 10 * time.Second

// These tests exercise a real etcd server through discovery into the routing
// cache. Each test owns an independent prefix and only removes its own keys.
// Run with AIBRIX_TEST_ETCD_ENDPOINT=http://127.0.0.1:2379.
func newEtcdDiscoveryTestClient(t *testing.T) (*clientv3.Client, string) {
	t.Helper()
	endpoint := os.Getenv("AIBRIX_TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("set AIBRIX_TEST_ETCD_ENDPOINT to run real-etcd discovery integration tests")
	}
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(endpoint, ","),
		DialTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	prefix := "/aibrix-tests/discovery/" + uuid.NewString() + "/"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := client.Delete(ctx, prefix, clientv3.WithPrefix())
		require.NoError(t, err)
	})
	return client, prefix
}

func startEtcdDiscoveryTestCache(t *testing.T, client *clientv3.Client, prefix string) *Store {
	t.Helper()
	provider, err := discovery.NewEtcdProvider(discovery.EtcdConfig{
		Endpoints:   client.Endpoints(),
		Prefix:      prefix,
		DialTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	store := NewForTest()
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	t.Cleanup(func() { stopOnce.Do(func() { close(stopCh) }) })
	require.NoError(t, initDiscoveryProvider(store, provider, stopCh))
	return store
}

func putEtcdDiscoveryTestEndpoint(
	t *testing.T, client *clientv3.Client, key string, endpoint discovery.EtcdEndpoint,
	opts ...clientv3.OpOption,
) {
	t.Helper()
	payload, err := json.Marshal(endpoint)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = client.Put(ctx, key, string(payload), opts...)
	require.NoError(t, err)
}

func deleteEtcdDiscoveryTestEndpoint(t *testing.T, client *clientv3.Client, key string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Delete(ctx, key)
	require.NoError(t, err)
}

func etcdDiscoveryTestPods(store *Store, model string) []*v1.Pod {
	pods, err := store.ListPodsByModel(model)
	if err != nil {
		return nil
	}
	return pods.All()
}

func awaitEtcdDiscoveryTestPods(t *testing.T, store *Store, model string, count int) []*v1.Pod {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(etcdDiscoveryTestPods(store, model)) == count
	}, etcdDiscoveryTestTimeout, 10*time.Millisecond, "model %q should have %d routing endpoints", model, count)
	return etcdDiscoveryTestPods(store, model)
}

func TestEtcdDiscoveryRoutingCacheLifecycle(t *testing.T) {
	client, prefix := newEtcdDiscoveryTestClient(t)
	endpoint := discovery.EtcdEndpoint{
		Model: "first-model", Address: "127.0.0.1:18000", Engine: "vllm",
	}
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"worker-1", endpoint)
	store := startEtcdDiscoveryTestCache(t, client, prefix)

	// Watch returning means its initial snapshot is already routable.
	require.ElementsMatch(t, []string{"first-model"}, store.ListModels())
	pods := etcdDiscoveryTestPods(store, "first-model")
	require.Len(t, pods, 1)
	initialPod := pods[0]
	require.Equal(t, "127.0.0.1", initialPod.Status.PodIP)
	require.Equal(t, "18000", initialPod.Labels[constants.ModelLabelPort])
	require.Equal(t, "vllm", initialPod.Labels[constants.ModelLabelEngine])
	require.NotEmpty(t, initialPod.UID)

	// A later key is a watch barrier: observing it proves the preceding
	// identical PUT was consumed before checking pointer stability.
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"worker-1", endpoint)
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"barrier", discovery.EtcdEndpoint{
		Model: "barrier-model", Address: "127.0.0.1:18999",
	})
	awaitEtcdDiscoveryTestPods(t, store, "barrier-model", 1)
	require.Same(t, initialPod, etcdDiscoveryTestPods(store, "first-model")[0],
		"an identical advertisement must not rebuild the cached pod")
	deleteEtcdDiscoveryTestEndpoint(t, client, prefix+"barrier")
	awaitEtcdDiscoveryTestPods(t, store, "barrier-model", 0)

	// Metadata changes update the existing endpoint identity.
	endpoint.Engine = "sglang"
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"worker-1", endpoint)
	require.Eventually(t, func() bool {
		current := etcdDiscoveryTestPods(store, "first-model")
		return len(current) == 1 && current[0].Labels[constants.ModelLabelEngine] == "sglang"
	}, etcdDiscoveryTestTimeout, 10*time.Millisecond)
	updated := etcdDiscoveryTestPods(store, "first-model")[0]
	require.Equal(t, initialPod.Name, updated.Name)
	require.Equal(t, initialPod.UID, updated.UID)

	putEtcdDiscoveryTestEndpoint(t, client, prefix+"worker-2", discovery.EtcdEndpoint{
		Model: "first-model", Address: "127.0.0.1:18001",
	})
	awaitEtcdDiscoveryTestPods(t, store, "first-model", 2)

	// Reassigning a registration must remove its old model mapping.
	endpoint.Model = "second-model"
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"worker-1", endpoint)
	awaitEtcdDiscoveryTestPods(t, store, "second-model", 1)
	remaining := awaitEtcdDiscoveryTestPods(t, store, "first-model", 1)
	require.Equal(t, "18001", remaining[0].Labels[constants.ModelLabelPort])
	require.ElementsMatch(t, []string{"first-model", "second-model"}, store.ListModels())

	// Retargeting the same key must replace the old backend address.
	endpoint.Address = "127.0.0.2:18002"
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"worker-1", endpoint)
	require.Eventually(t, func() bool {
		current := etcdDiscoveryTestPods(store, "second-model")
		return len(current) == 1 && current[0].Status.PodIP == "127.0.0.2" &&
			current[0].Labels[constants.ModelLabelPort] == "18002"
	}, etcdDiscoveryTestTimeout, 10*time.Millisecond)

	deleteEtcdDiscoveryTestEndpoint(t, client, prefix+"worker-2")
	awaitEtcdDiscoveryTestPods(t, store, "first-model", 0)
	require.NotContains(t, store.ListModels(), "first-model")

	// Invalid replacement data withdraws the formerly valid advertisement.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := client.Put(ctx, prefix+"worker-1", `{"model":"second-model","address":"127.0.0.2:invalid"}`)
	cancel()
	require.NoError(t, err)
	awaitEtcdDiscoveryTestPods(t, store, "second-model", 0)
	require.Empty(t, store.ListModels())

	// A corrected registration becomes routable without a restart.
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"worker-1", endpoint)
	awaitEtcdDiscoveryTestPods(t, store, "second-model", 1)
	deleteEtcdDiscoveryTestEndpoint(t, client, prefix+"worker-1")
	awaitEtcdDiscoveryTestPods(t, store, "second-model", 0)
	require.Empty(t, store.ListModels())

	// Role metadata reaches the same pod objects used by the PD router.
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"prefill", discovery.EtcdEndpoint{
		Model: "pd-model", Address: "127.0.0.1:18100", Engine: "vllm",
		Role: "prefill", RoleSet: "pair-a",
	})
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"decode", discovery.EtcdEndpoint{
		Model: "pd-model", Address: "127.0.0.1:18200", Engine: "vllm",
		Role: "decode", RoleSet: "pair-a",
	})
	pdPods := awaitEtcdDiscoveryTestPods(t, store, "pd-model", 2)
	roles := make(map[string]string)
	for _, pod := range pdPods {
		require.Equal(t, "pair-a", pod.Labels["roleset-name"])
		roles[pod.Labels["role-name"]] = pod.Labels[constants.ModelLabelPort]
	}
	require.Equal(t, map[string]string{"prefill": "18100", "decode": "18200"}, roles)

	// A new gateway cache recovers current registrations using only etcd.
	reloaded := startEtcdDiscoveryTestCache(t, client, prefix)
	require.ElementsMatch(t, []string{"pd-model"}, reloaded.ListModels())
	require.Len(t, etcdDiscoveryTestPods(reloaded, "pd-model"), 2)
}

func TestEtcdDiscoveryRoutingCacheLeaseExpiry(t *testing.T) {
	client, prefix := newEtcdDiscoveryTestClient(t)
	store := startEtcdDiscoveryTestCache(t, client, prefix)
	require.Empty(t, store.ListModels())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	lease, err := client.Grant(ctx, 2)
	cancel()
	require.NoError(t, err)
	putEtcdDiscoveryTestEndpoint(t, client, prefix+"leased-worker", discovery.EtcdEndpoint{
		Model: "leased-model", Address: "127.0.0.1:18300",
	}, clientv3.WithLease(lease.ID))
	awaitEtcdDiscoveryTestPods(t, store, "leased-model", 1)

	// No revoke or explicit delete: real server TTL expiry must remove the
	// endpoint and the now-empty model from the routing cache.
	awaitEtcdDiscoveryTestPods(t, store, "leased-model", 0)
	require.NotContains(t, store.ListModels(), "leased-model")
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	result, err := client.Get(ctx, prefix+"leased-worker")
	cancel()
	require.NoError(t, err)
	require.Empty(t, result.Kvs)
}
