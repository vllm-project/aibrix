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
	"crypto/tls"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/metadata"
	v1 "k8s.io/api/core/v1"
)

func TestEtcdEndpointPodValidation(t *testing.T) {
	valid := EtcdEndpoint{Model: "Qwen/Qwen2.5-72B", Address: "worker:8000"}
	for _, test := range []struct {
		name     string
		endpoint EtcdEndpoint
	}{
		{"empty model", EtcdEndpoint{Address: "worker:8000"}},
		{"blank model", EtcdEndpoint{Model: " \t", Address: "worker:8000"}},
		{"empty address", EtcdEndpoint{Model: "model"}},
		{"missing port", EtcdEndpoint{Model: "model", Address: "worker"}},
		{"empty host", EtcdEndpoint{Model: "model", Address: ":8000"}},
		{"empty port", EtcdEndpoint{Model: "model", Address: "worker:"}},
		{"zero port", EtcdEndpoint{Model: "model", Address: "worker:0"}},
		{"negative port", EtcdEndpoint{Model: "model", Address: "worker:-1"}},
		{"signed port", EtcdEndpoint{Model: "model", Address: "worker:+1"}},
		{"large port", EtcdEndpoint{Model: "model", Address: "worker:65536"}},
		{"named port", EtcdEndpoint{Model: "model", Address: "worker:http"}},
		{"URL", EtcdEndpoint{Model: "model", Address: "http://worker:8000"}},
		{"path", EtcdEndpoint{Model: "model", Address: "worker/path:8000"}},
		{"credentials", EtcdEndpoint{Model: "model", Address: "secret@worker:8000"}},
		{"whitespace host", EtcdEndpoint{Model: "model", Address: " worker:8000"}},
		{"unbracketed IPv6", EtcdEndpoint{Model: "model", Address: "2001:db8::1:8000"}},
		{"bad role", EtcdEndpoint{Model: "model", Address: "worker:8000", Role: "both", RoleSet: "group"}},
		{"role only", EtcdEndpoint{Model: "model", Address: "worker:8000", Role: "prefill"}},
		{"roleset only", EtcdEndpoint{Model: "model", Address: "worker:8000", RoleSet: "group"}},
		{"blank roleset", EtcdEndpoint{Model: "model", Address: "worker:8000", Role: "decode", RoleSet: " "}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod, err := etcdEndpointPod(etcdKV(t, "key", test.endpoint, 1, 1))
			require.Error(t, err)
			assert.Nil(t, pod)
		})
	}
	for _, value := range []string{"", "null", "[]", "42", `{"model":"secret","address":false}`, "private-secret-value"} {
		_, err := etcdEndpointPod(&mvccpb.KeyValue{Key: []byte("private-key"), Value: []byte(value)})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret")
	}
	for _, address := range []string{"worker:8000", "worker.example.:08000", "127.0.0.1:1", "[2001:db8::1]:65535"} {
		t.Run(address, func(t *testing.T) {
			endpoint := valid
			endpoint.Address = address
			endpoint.Engine, endpoint.Role, endpoint.RoleSet = "vllm", "prefill", "rack-1"
			pod, err := etcdEndpointPod(etcdKV(t, "key", endpoint, 1, 1))
			require.NoError(t, err)
			assert.Equal(t, endpoint.Model, pod.Labels[constants.ModelLabelName])
			assert.Equal(t, "vllm", pod.Labels[constants.ModelLabelEngine])
			assert.Equal(t, "prefill", pod.Labels["role-name"])
			assert.Equal(t, "rack-1", pod.Labels["roleset-name"])
			assert.Equal(t, standaloneNamespace, pod.Namespace)
			assert.Equal(t, v1.PodRunning, pod.Status.Phase)
			assert.Equal(t, v1.ConditionTrue, pod.Status.Conditions[0].Status)
			assert.NotEmpty(t, pod.UID)
		})
	}
}

func TestEtcdEndpointIdentity(t *testing.T) {
	endpoint := EtcdEndpoint{Model: "model", Address: "worker:8000", Engine: "vllm"}
	original, err := etcdEndpointPod(etcdKV(t, "key", endpoint, 1, 2))
	require.NoError(t, err)
	same, err := etcdEndpointPod(etcdKV(t, "key", endpoint, 1, 20))
	require.NoError(t, err)
	assert.Equal(t, original, same, "ordinary PUT revisions must not change route identity")
	endpoint.Engine = "sglang"
	updated, err := etcdEndpointPod(etcdKV(t, "key", endpoint, 1, 21))
	require.NoError(t, err)
	assert.Equal(t, original.Name, updated.Name)
	assert.Equal(t, original.UID, updated.UID)
	recreated, err := etcdEndpointPod(etcdKV(t, "key", endpoint, 22, 22))
	require.NoError(t, err)
	assert.Equal(t, original.Name, recreated.Name)
	assert.NotEqual(t, original.UID, recreated.UID)
	for _, test := range []struct {
		key      string
		endpoint EtcdEndpoint
	}{
		{"other-key", endpoint},
		{"key", EtcdEndpoint{Model: "other-model", Address: "worker:8000"}},
		{"key", EtcdEndpoint{Model: "model", Address: "other-worker:8000"}},
		{"key", EtcdEndpoint{Model: "model", Address: "worker:8001"}},
		{"key", EtcdEndpoint{Model: "model", Address: "worker:8000", Role: "prefill", RoleSet: "a"}},
		{"key", EtcdEndpoint{Model: "model", Address: "worker:8000", Role: "decode", RoleSet: "a"}},
		{"key", EtcdEndpoint{Model: "model", Address: "worker:8000", Role: "decode", RoleSet: "b"}},
	} {
		pod, err := etcdEndpointPod(etcdKV(t, test.key, test.endpoint, 1, 2))
		require.NoError(t, err)
		assert.NotEqual(t, original.Name, pod.Name)
	}
	endpoint.Address = "WORKER.:08000"
	canonical, err := etcdEndpointPod(etcdKV(t, "key", endpoint, 1, 22))
	require.NoError(t, err)
	assert.Equal(t, original.Name, canonical.Name)
}

func TestEtcdProviderConfig(t *testing.T) {
	for _, config := range []EtcdConfig{{}, {Endpoints: []string{" "}}, {Endpoints: []string{"localhost:2379"}, DialTimeout: -1}} {
		_, err := NewEtcdProvider(config)
		require.Error(t, err)
	}
	config := EtcdConfig{Endpoints: []string{"localhost:2379"}, TLS: &tls.Config{MinVersion: tls.VersionTLS12}}
	provider, err := NewEtcdProvider(config)
	require.NoError(t, err)
	assert.Equal(t, "etcd", provider.Type())
	assert.Equal(t, defaultEtcdPrefix, provider.config.Prefix)
	assert.Equal(t, defaultEtcdDialTimeout, provider.config.DialTimeout)
	config.Endpoints[0] = "modified"
	config.TLS.MinVersion = tls.VersionTLS13
	assert.Equal(t, "localhost:2379", provider.config.Endpoints[0])
	assert.Equal(t, uint16(tls.VersionTLS12), provider.config.TLS.MinVersion)
	require.Error(t, provider.Watch(nil, nil))
}

func TestEtcdProviderSnapshotAndEvents(t *testing.T) {
	client := newFakeEtcdClient()
	provider := fakeEtcdProvider(t, client)
	endpoint := EtcdEndpoint{Model: "model", Address: "worker:8000", Engine: "vllm"}
	initial := etcdKV(t, "a", endpoint, 1, 10)
	invalid := &mvccpb.KeyValue{Key: []byte("bad"), Value: []byte("invalid"), ModRevision: 10}
	stop, events, stream := startFakeEtcdWatch(t, provider, client, 10, initial, invalid)
	defer stop()
	added := receiveEtcd(t, events)
	assert.Equal(t, EventAdd, added.Type)
	assert.Equal(t, "worker", added.Object.(*v1.Pod).Status.PodIP)
	// A callback may mutate its objects; provider-owned state must remain intact.
	added.Object.(*v1.Pod).Labels[constants.ModelLabelEngine] = "callback mutation"

	endpoint.Engine = "sglang"
	other := EtcdEndpoint{Model: "other", Address: "other:9000"}
	// Both events share a transaction revision. Neither may be skipped.
	stream <- etcdWatchResponse(11, etcdPut(etcdKV(t, "a", endpoint, 1, 11)), etcdPut(etcdKV(t, "b", other, 11, 11)))
	updated := receiveEtcd(t, events)
	assert.Equal(t, EventUpdate, updated.Type)
	assert.Equal(t, "vllm", updated.OldObject.(*v1.Pod).Labels[constants.ModelLabelEngine])
	assert.Equal(t, "sglang", updated.Object.(*v1.Pod).Labels[constants.ModelLabelEngine])
	assert.Equal(t, EventAdd, receiveEtcd(t, events).Type)
	// An identical value with a newer PUT revision emits no update.
	stream <- etcdWatchResponse(12, etcdPut(etcdKV(t, "a", endpoint, 1, 12)))
	endpoint.Model = "new-model"
	stream <- etcdWatchResponse(13, etcdPut(etcdKV(t, "a", endpoint, 1, 13)))
	deleted, added := receiveEtcd(t, events), receiveEtcd(t, events)
	assert.Equal(t, EventDelete, deleted.Type)
	assert.Equal(t, "model", deleted.Object.(*v1.Pod).Labels[constants.ModelLabelName])
	assert.Equal(t, EventAdd, added.Type)
	assert.Equal(t, "new-model", added.Object.(*v1.Pod).Labels[constants.ModelLabelName])
	assert.NotEqual(t, deleted.Object.(*v1.Pod).Name, added.Object.(*v1.Pod).Name)
	// Invalid replacement removes the old backend without blocking other keys.
	invalid.Key, invalid.ModRevision = []byte("b"), 14
	stream <- etcdWatchResponse(14, etcdPut(invalid))
	deleted = receiveEtcd(t, events)
	assert.Equal(t, EventDelete, deleted.Type)
	assert.Equal(t, "other", deleted.Object.(*v1.Pod).Labels[constants.ModelLabelName])
	// A lease expiration has the same DELETE event as an explicit deletion.
	stream <- etcdWatchResponse(15, etcdDelete("a", 15))
	assert.Equal(t, EventDelete, receiveEtcd(t, events).Type)
	stream <- etcdWatchResponse(16, etcdDelete("a", 16), etcdPut(etcdKV(t, "marker", other, 16, 16)))
	assert.Equal(t, EventAdd, receiveEtcd(t, events).Type)
	assert.Empty(t, events)
	stop()
	receiveEtcd(t, client.closed)
	require.Error(t, provider.Watch(func(WatchEvent) {}, nil))
}

func TestEtcdProviderChangesDuringInitialDelivery(t *testing.T) {
	client := newFakeEtcdClient()
	provider := fakeEtcdProvider(t, client)
	endpoint := EtcdEndpoint{Model: "model", Address: "worker:8000", Engine: "vllm"}
	stopCh := make(chan struct{})
	defer close(stopCh)
	initialEntered, releaseInitial := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseInitial) })
	events, result := make(chan WatchEvent, 10), make(chan error, 1)
	go func() {
		result <- provider.Watch(func(event WatchEvent) {
			if event.Type == EventAdd {
				close(initialEntered)
				<-releaseInitial
			}
			events <- event
		}, stopCh)
	}()
	request := receiveEtcd(t, client.gets)
	request.reply <- fakeEtcdGetResult{response: etcdSnapshot(10, etcdKV(t, "key", endpoint, 1, 10))}
	watch := receiveEtcd(t, client.watches)
	receiveEtcd(t, initialEntered)
	// etcd changes after the snapshot, while initial callbacks are still running.
	endpoint.Engine = "sglang"
	watch.stream <- etcdWatchResponse(11, etcdPut(etcdKV(t, "key", endpoint, 1, 11)))
	assert.Empty(t, events, "ongoing callbacks must not run concurrently with initial delivery")
	assert.Empty(t, result, "Watch readiness waits for initial delivery")
	releaseOnce.Do(func() { close(releaseInitial) })
	require.NoError(t, receiveEtcd(t, result))
	assert.Equal(t, EventAdd, receiveEtcd(t, events).Type)
	updated := receiveEtcd(t, events)
	assert.Equal(t, EventUpdate, updated.Type)
	assert.Equal(t, "vllm", updated.OldObject.(*v1.Pod).Labels[constants.ModelLabelEngine])
	assert.Equal(t, "sglang", updated.Object.(*v1.Pod).Labels[constants.ModelLabelEngine])
	assert.Equal(t, int64(11), watch.op.Rev())
}

func TestEtcdProviderReconnectAndCompaction(t *testing.T) {
	client := newFakeEtcdClient()
	provider := fakeEtcdProvider(t, client)
	a := EtcdEndpoint{Model: "a", Address: "a:8000"}
	b := EtcdEndpoint{Model: "b", Address: "b:8000"}
	c := EtcdEndpoint{Model: "c", Address: "c:8000"}
	stop, events, stream := startFakeEtcdWatch(t, provider, client, 10,
		etcdKV(t, "a", a, 1, 10), etcdKV(t, "b", b, 2, 10), etcdKV(t, "c", c, 3, 10))
	defer stop()
	for range 3 {
		receiveEtcd(t, events)
	}
	// Creation header can be ahead of historical watch events. Reconnect must
	// start at the last applied revision, not this header.
	stream <- clientv3.WatchResponse{Header: etcdserverpb.ResponseHeader{Revision: 100}, Created: true}
	close(stream)
	request := receiveEtcd(t, client.watches)
	assert.Equal(t, int64(11), request.op.Rev())
	assertEtcdRequireLeader(t, request.ctx)
	stream = request.stream
	b.Engine = "vllm"
	stream <- etcdWatchResponse(100, etcdPut(etcdKV(t, "a", a, 1, 11)), etcdPut(etcdKV(t, "b", b, 2, 11)))
	assert.Equal(t, EventUpdate, receiveEtcd(t, events).Type)
	close(stream)
	request = receiveEtcd(t, client.watches)
	assert.Equal(t, int64(12), request.op.Rev(), "header must not hide unapplied revisions")
	stream = request.stream
	d := EtcdEndpoint{Model: "d", Address: "d:8000"}
	stream <- etcdWatchResponse(12, etcdPut(etcdKV(t, "b", b, 2, 11)), etcdPut(etcdKV(t, "d", d, 12, 12)))
	assert.Equal(t, EventAdd, receiveEtcd(t, events).Type)
	stream <- clientv3.WatchResponse{Canceled: true, CompactRevision: 50}
	resync := receiveEtcd(t, client.gets)
	resync.reply <- fakeEtcdGetResult{err: errors.New("temporary outage")}
	// A failed resnapshot must preserve all last-known state and retry.
	resync = receiveEtcd(t, client.gets)
	assert.Empty(t, events)
	d.Role, d.RoleSet = "decode", "group"
	e := EtcdEndpoint{Model: "e", Address: "e:8000"}
	resync.reply <- fakeEtcdGetResult{response: etcdSnapshot(60,
		etcdKV(t, "a", a, 1, 58),  // Unchanged backend: no event despite PUT revision.
		etcdKV(t, "c", c, 55, 55), // Same key/value, new registration lifetime.
		etcdKV(t, "d", d, 12, 59), // Routing identity changed.
		etcdKV(t, "e", e, 60, 60))}
	for _, expected := range []struct {
		eventType EventType
		model     string
	}{
		{EventDelete, "b"}, {EventDelete, "c"}, {EventAdd, "c"},
		{EventDelete, "d"}, {EventAdd, "d"}, {EventAdd, "e"},
	} {
		event := receiveEtcd(t, events)
		assert.Equal(t, expected.eventType, event.Type)
		assert.Equal(t, expected.model, event.Object.(*v1.Pod).Labels[constants.ModelLabelName])
	}
	request = receiveEtcd(t, client.watches)
	assert.Equal(t, int64(61), request.op.Rev())
	assertEtcdRequireLeader(t, request.ctx)
	request.stream <- etcdWatchResponse(61, etcdDelete("e", 61))
	assert.Equal(t, EventDelete, receiveEtcd(t, events).Type)
	assert.Empty(t, events)
}

func TestEtcdProviderWatchLifecycle(t *testing.T) {
	t.Run("already stopped", func(t *testing.T) {
		provider, err := NewEtcdProvider(EtcdConfig{Endpoints: []string{"localhost:2379"}})
		require.NoError(t, err)
		provider.newClient = func(clientv3.Config) (etcdClient, error) {
			t.Error("must not create a client after stop")
			return nil, errors.New("unexpected call")
		}
		stop := make(chan struct{})
		close(stop)
		require.ErrorIs(t, provider.Watch(func(WatchEvent) { t.Error("unexpected callback") }, stop), context.Canceled)
	})
	t.Run("client creation error", func(t *testing.T) {
		provider, err := NewEtcdProvider(EtcdConfig{Endpoints: []string{"localhost:2379"}})
		require.NoError(t, err)
		var ctx context.Context
		provider.newClient = func(config clientv3.Config) (etcdClient, error) {
			ctx = config.Context
			return nil, errors.New("cannot create client")
		}
		require.ErrorContains(t, provider.Watch(func(WatchEvent) {}, nil), "create etcd discovery client")
		receiveEtcd(t, ctx.Done())
	})
	for _, scenario := range []string{"snapshot error", "snapshot timeout", "stop during snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			client := newFakeEtcdClient()
			provider := fakeEtcdProvider(t, client)
			if scenario == "snapshot timeout" {
				provider.config.DialTimeout = 20 * time.Millisecond
			}
			stopCh := make(chan struct{})
			defer func() {
				select {
				case <-stopCh:
				default:
					close(stopCh)
				}
			}()
			result := make(chan error, 1)
			go func() { result <- provider.Watch(func(WatchEvent) { t.Error("unexpected callback") }, stopCh) }()
			request := receiveEtcd(t, client.gets)
			switch scenario {
			case "snapshot error":
				request.reply <- fakeEtcdGetResult{err: errors.New("snapshot unavailable")}
			case "stop during snapshot":
				close(stopCh)
			}
			require.Error(t, receiveEtcd(t, result))
			receiveEtcd(t, client.closed)
			receiveEtcd(t, request.ctx.Done())
			assert.Empty(t, client.watches)
		})
	}
	t.Run("stop cancels watch", func(t *testing.T) {
		client := newFakeEtcdClient()
		provider := fakeEtcdProvider(t, client)
		stop, _, _ := startFakeEtcdWatch(t, provider, client, 10)
		stop()
		receiveEtcd(t, client.closed)
		receiveEtcd(t, client.lastWatchContext.Done())
	})
	t.Run("stop cancels resnapshot", func(t *testing.T) {
		client := newFakeEtcdClient()
		provider := fakeEtcdProvider(t, client)
		stop, _, stream := startFakeEtcdWatch(t, provider, client, 10)
		defer stop()
		stream <- clientv3.WatchResponse{Canceled: true, CompactRevision: 20}
		request := receiveEtcd(t, client.gets)
		stop()
		receiveEtcd(t, client.closed)
		receiveEtcd(t, request.ctx.Done())
	})
	t.Run("stop cancels reconnect backoff", func(t *testing.T) {
		client := newFakeEtcdClient()
		provider := fakeEtcdProvider(t, client)
		stop, _, stream := startFakeEtcdWatch(t, provider, client, 10)
		close(stream)
		stop()
		receiveEtcd(t, client.closed)
		assert.Empty(t, client.watches)
	})
}

func TestEtcdProviderPassesConnectionConfig(t *testing.T) {
	config := EtcdConfig{
		Endpoints: []string{"https://etcd.example:2379"}, Prefix: "/custom/",
		DialTimeout: 3 * time.Second, Username: "user", Password: "password",
		TLS: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "etcd.example"},
	}
	provider, err := NewEtcdProvider(config)
	require.NoError(t, err)
	client := newFakeEtcdClient()
	provider.newClient = func(actual clientv3.Config) (etcdClient, error) {
		assert.Equal(t, config.Endpoints, actual.Endpoints)
		assert.Equal(t, config.DialTimeout, actual.DialTimeout)
		assert.Equal(t, config.Username, actual.Username)
		assert.Equal(t, config.Password, actual.Password)
		assert.Equal(t, config.TLS.ServerName, actual.TLS.ServerName)
		return client, nil
	}
	stop, _, _ := startFakeEtcdWatch(t, provider, client, 1)
	stop()
	receiveEtcd(t, client.closed)
}

func etcdKV(t *testing.T, key string, endpoint EtcdEndpoint, createRevision, modRevision int64) *mvccpb.KeyValue {
	t.Helper()
	value, err := json.Marshal(endpoint)
	require.NoError(t, err)
	return &mvccpb.KeyValue{Key: []byte(key), Value: value, CreateRevision: createRevision, ModRevision: modRevision}
}

func etcdSnapshot(revision int64, kvs ...*mvccpb.KeyValue) *clientv3.GetResponse {
	return &clientv3.GetResponse{Header: &etcdserverpb.ResponseHeader{Revision: revision}, Kvs: kvs}
}

func etcdPut(kv *mvccpb.KeyValue) *clientv3.Event { return &clientv3.Event{Type: mvccpb.PUT, Kv: kv} }
func etcdDelete(key string, revision int64) *clientv3.Event {
	return &clientv3.Event{Type: mvccpb.DELETE, Kv: &mvccpb.KeyValue{Key: []byte(key), ModRevision: revision}}
}
func etcdWatchResponse(revision int64, events ...*clientv3.Event) clientv3.WatchResponse {
	return clientv3.WatchResponse{Header: etcdserverpb.ResponseHeader{Revision: revision}, Events: events}
}

type fakeEtcdGetResult struct {
	response *clientv3.GetResponse
	err      error
}
type fakeEtcdGetRequest struct {
	ctx   context.Context
	op    clientv3.Op
	reply chan fakeEtcdGetResult
}
type fakeEtcdWatchRequest struct {
	ctx    context.Context
	op     clientv3.Op
	stream chan clientv3.WatchResponse
}
type fakeEtcdClient struct {
	gets             chan fakeEtcdGetRequest
	watches          chan fakeEtcdWatchRequest
	closed           chan struct{}
	once             sync.Once
	lastWatchContext context.Context
}

func newFakeEtcdClient() *fakeEtcdClient {
	return &fakeEtcdClient{
		gets: make(chan fakeEtcdGetRequest, 10), watches: make(chan fakeEtcdWatchRequest, 10), closed: make(chan struct{}),
	}
}
func (f *fakeEtcdClient) Get(ctx context.Context, key string, options ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	request := fakeEtcdGetRequest{ctx: ctx, op: clientv3.OpGet(key, options...), reply: make(chan fakeEtcdGetResult, 1)}
	select {
	case f.gets <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case result := <-request.reply:
		return result.response, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *fakeEtcdClient) Watch(ctx context.Context, key string, options ...clientv3.OpOption) clientv3.WatchChan {
	request := fakeEtcdWatchRequest{ctx: ctx, op: clientv3.OpGet(key, options...), stream: make(chan clientv3.WatchResponse, 10)}
	f.lastWatchContext = ctx
	select {
	case f.watches <- request:
	case <-ctx.Done():
	}
	return request.stream
}
func (f *fakeEtcdClient) Close() error { f.once.Do(func() { close(f.closed) }); return nil }

func fakeEtcdProvider(t *testing.T, client *fakeEtcdClient) *EtcdProvider {
	t.Helper()
	provider, err := NewEtcdProvider(EtcdConfig{Endpoints: []string{"localhost:2379"}})
	require.NoError(t, err)
	provider.newClient = func(clientv3.Config) (etcdClient, error) { return client, nil }
	return provider
}

func startFakeEtcdWatch(t *testing.T, provider *EtcdProvider, client *fakeEtcdClient, revision int64,
	kvs ...*mvccpb.KeyValue,
) (func(), chan WatchEvent, chan clientv3.WatchResponse) {
	t.Helper()
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }
	t.Cleanup(func() { stop(); receiveEtcd(t, client.closed) })
	events := make(chan WatchEvent, 100)
	result := make(chan error, 1)
	go func() { result <- provider.Watch(func(event WatchEvent) { events <- event }, stopCh) }()
	request := receiveEtcd(t, client.gets)
	assert.Equal(t, provider.config.Prefix, string(request.op.KeyBytes()))
	assert.Equal(t, clientv3.GetPrefixRangeEnd(provider.config.Prefix), string(request.op.RangeBytes()))
	request.reply <- fakeEtcdGetResult{response: etcdSnapshot(revision, kvs...)}
	watch := receiveEtcd(t, client.watches)
	assert.Equal(t, provider.config.Prefix, string(watch.op.KeyBytes()))
	assert.Equal(t, clientv3.GetPrefixRangeEnd(provider.config.Prefix), string(watch.op.RangeBytes()))
	assert.Equal(t, revision+1, watch.op.Rev())
	assertEtcdRequireLeader(t, watch.ctx)
	require.NoError(t, receiveEtcd(t, result))
	return stop, events, watch.stream
}

func receiveEtcd[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %T", channel)
		var zero T
		return zero
	}
}

func assertEtcdRequireLeader(t *testing.T, ctx context.Context) {
	t.Helper()
	md, ok := metadata.FromOutgoingContext(ctx)
	require.True(t, ok)
	assert.Equal(t, []string{rpctypes.MetadataHasLeader}, md.Get(rpctypes.MetadataRequireLeaderKey))
}
