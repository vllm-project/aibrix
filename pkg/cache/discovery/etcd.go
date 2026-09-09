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
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
)

const (
	defaultEtcdPrefix      = "/aibrix/endpoints/"
	defaultEtcdDialTimeout = 5 * time.Second
	etcdRetryDelay         = time.Second
)

// EtcdConfig configures an etcd endpoint registry. Credentials and TLS are optional.
type EtcdConfig struct {
	Endpoints []string
	// Prefix is the key prefix to watch. It defaults to /aibrix/endpoints/.
	Prefix string
	// DialTimeout also bounds each snapshot request. It defaults to five seconds.
	DialTimeout time.Duration
	Username    string
	Password    string
	TLS         *tls.Config
}

// EtcdEndpoint is the JSON value registered under each key in the discovery prefix.
// Each key describes one backend. A registrar may attach an etcd lease to the key
// so that an expired registration automatically removes the backend.
type EtcdEndpoint struct {
	Model string `json:"model"`
	// Address must be host:port, or [IPv6]:port. Ports must be between 1 and 65535.
	Address string `json:"address"`
	Engine  string `json:"engine,omitempty"`
	// Role and RoleSet must be specified together. Role is prefill or decode.
	Role    string `json:"role,omitempty"`
	RoleSet string `json:"roleset,omitempty"`
}

// etcdClient is the part of clientv3.Client used by discovery.
type etcdClient interface {
	Get(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error)
	Watch(context.Context, string, ...clientv3.OpOption) clientv3.WatchChan
	Close() error
}

// EtcdProvider discovers backends from an etcd prefix. Watch may be called once.
// The client is created by Watch and is closed when stopCh closes, or if initial
// synchronization fails. After a successful initial sync, temporary outages keep
// the last known backends until the watch can resume or reconcile a new snapshot.
type EtcdProvider struct {
	config    EtcdConfig
	newClient func(clientv3.Config) (etcdClient, error)
	mu        sync.Mutex
	started   bool
}

// NewEtcdProvider validates configuration without opening a connection.
func NewEtcdProvider(config EtcdConfig) (*EtcdProvider, error) {
	if len(config.Endpoints) == 0 {
		return nil, fmt.Errorf("etcd discovery requires at least one etcd endpoint")
	}
	for _, endpoint := range config.Endpoints {
		if strings.TrimSpace(endpoint) == "" {
			return nil, fmt.Errorf("etcd discovery endpoints must not be empty")
		}
	}
	if config.Prefix == "" {
		config.Prefix = defaultEtcdPrefix
	}
	if config.DialTimeout < 0 {
		return nil, fmt.Errorf("etcd discovery dial timeout must be positive")
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = defaultEtcdDialTimeout
	}
	config.Endpoints = append([]string(nil), config.Endpoints...)
	if config.TLS != nil {
		config.TLS = config.TLS.Clone()
	}
	return &EtcdProvider{
		config: config,
		newClient: func(config clientv3.Config) (etcdClient, error) {
			return clientv3.New(config)
		},
	}, nil
}

// Type returns the provider type identifier.
func (p *EtcdProvider) Type() string { return "etcd" }

// Watch delivers an atomic initial snapshot and then serially delivers changes.
// Watching starts at snapshot revision + 1, including changes made during sync.
func (p *EtcdProvider) Watch(handler EventHandler, stopCh <-chan struct{}) error {
	if handler == nil {
		return fmt.Errorf("etcd discovery requires an event handler")
	}
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return fmt.Errorf("etcd discovery Watch may only be called once")
	}
	p.started = true
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	select {
	case <-stopCh:
		cancel()
		return context.Canceled
	default:
	}
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	client, err := p.newClient(clientv3.Config{
		Endpoints:   p.config.Endpoints,
		DialTimeout: p.config.DialTimeout,
		Username:    p.config.Username,
		Password:    p.config.Password,
		TLS:         p.config.TLS,
		Context:     ctx,
	})
	if err != nil {
		cancel()
		return fmt.Errorf("create etcd discovery client: %w", err)
	}
	cleanup := func() {
		cancel()
		_ = client.Close()
	}
	pods, revision, err := p.snapshot(ctx, client)
	if err != nil {
		cleanup()
		return fmt.Errorf("initial etcd discovery snapshot: %w", err)
	}
	// Requiring a leader prevents a watch from silently stalling on an isolated member.
	watchCtx, stopWatch := context.WithCancel(clientv3.WithRequireLeader(ctx))
	stream := client.Watch(watchCtx, p.config.Prefix, clientv3.WithPrefix(), clientv3.WithRev(revision+1))
	state := make(map[string]*v1.Pod)
	if err := reconcileEtcdPods(ctx, handler, state, pods); err != nil {
		stopWatch()
		cleanup()
		return err
	}
	go func() {
		defer cleanup()
		p.run(ctx, client, handler, state, revision, stream, stopWatch)
	}()
	return nil
}

func (p *EtcdProvider) snapshot(ctx context.Context, client etcdClient) (map[string]*v1.Pod, int64, error) {
	requestCtx, cancel := context.WithTimeout(ctx, p.config.DialTimeout)
	defer cancel()
	response, err := client.Get(requestCtx, p.config.Prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, 0, err
	}
	pods := make(map[string]*v1.Pod, len(response.Kvs))
	for _, kv := range response.Kvs {
		if pod := registeredEtcdPod(kv); pod != nil {
			pods[string(kv.Key)] = pod
		}
	}
	return pods, response.Header.Revision, nil
}

func (p *EtcdProvider) run(ctx context.Context, client etcdClient, handler EventHandler,
	state map[string]*v1.Pod, revision int64, stream clientv3.WatchChan, stopWatch context.CancelFunc,
) {
	defer func() { stopWatch() }()
	resync := false
	for {
		select {
		case <-ctx.Done():
			return
		case response, ok := <-stream:
			if ok && response.Err() == nil && !response.Canceled {
				nextRevision := revision
				for _, event := range response.Events {
					if event.Kv == nil || event.Kv.ModRevision <= revision {
						continue
					}
					var pod *v1.Pod
					if event.Type == mvccpb.PUT {
						pod = registeredEtcdPod(event.Kv)
					}
					if err := replaceEtcdPod(ctx, handler, state, string(event.Kv.Key), pod); err != nil {
						return
					}
					if event.Kv.ModRevision > nextRevision {
						nextRevision = event.Kv.ModRevision
					}
				}
				// A watch-created response can have a header ahead of its events.
				// Advance only past events actually applied, after the whole batch.
				// The client coalesces fragmented responses by default.
				revision = nextRevision
				continue
			}
			if ctx.Err() != nil {
				return
			}
			resync = response.CompactRevision > 0
			klog.InfoS("Etcd discovery watch interrupted; reconnecting", "compacted", resync)
		}

		stopWatch()
		for {
			if !waitEtcdRetry(ctx) {
				return
			}
			if resync {
				pods, snapshotRevision, err := p.snapshot(ctx, client)
				if err != nil {
					// Values, keys, endpoints, and credentials never enter this log.
					klog.InfoS("Etcd discovery snapshot unavailable; retrying")
					continue
				}
				if err := reconcileEtcdPods(ctx, handler, state, pods); err != nil {
					return
				}
				revision = snapshotRevision
			}
			watchCtx, cancel := context.WithCancel(clientv3.WithRequireLeader(ctx))
			stopWatch = cancel
			stream = client.Watch(watchCtx, p.config.Prefix, clientv3.WithPrefix(), clientv3.WithRev(revision+1))
			break
		}
	}
}

func waitEtcdRetry(ctx context.Context) bool {
	timer := time.NewTimer(etcdRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// reconcileEtcdPods emits only actual differences, preserving unchanged pods and
// their router statistics during full resynchronization after compaction.
func reconcileEtcdPods(ctx context.Context, handler EventHandler, state, next map[string]*v1.Pod) error {
	keys := make([]string, 0, len(state)+len(next))
	for key := range state {
		if _, exists := next[key]; !exists {
			keys = append(keys, key)
		}
	}
	for key := range next {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := replaceEtcdPod(ctx, handler, state, key, next[key]); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func replaceEtcdPod(ctx context.Context, handler EventHandler, state map[string]*v1.Pod, key string, next *v1.Pod) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	old := state[key]
	if reflect.DeepEqual(old, next) {
		return nil
	}
	if old != nil && (next == nil || old.Name != next.Name || old.UID != next.UID) {
		handler(WatchEvent{Type: EventDelete, Object: old.DeepCopy()})
		delete(state, key)
		old = nil
	}
	if next != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		if old == nil {
			handler(WatchEvent{Type: EventAdd, Object: next.DeepCopy()})
		} else {
			handler(WatchEvent{Type: EventUpdate, Object: next.DeepCopy(), OldObject: old.DeepCopy()})
		}
		state[key] = next
	}
	return nil
}

func registeredEtcdPod(kv *mvccpb.KeyValue) *v1.Pod {
	pod, err := etcdEndpointPod(kv)
	if err != nil {
		// Key/value bytes can contain credentials or arbitrary input. Log only
		// an opaque key identifier and a validation error without input values.
		keyID := fmt.Sprintf("%x", sha256.Sum256(kv.Key))
		klog.ErrorS(err, "Ignoring invalid etcd discovery registration", "keyHash", keyID)
		return nil
	}
	return pod
}

func etcdEndpointPod(kv *mvccpb.KeyValue) (*v1.Pod, error) {
	var endpoint EtcdEndpoint
	if err := json.Unmarshal(kv.Value, &endpoint); err != nil {
		return nil, fmt.Errorf("registration must be a JSON endpoint object")
	}
	if strings.TrimSpace(endpoint.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}
	if (endpoint.Role == "") != (endpoint.RoleSet == "") {
		return nil, fmt.Errorf("role and roleset must be specified together")
	}
	if endpoint.Role != "" && endpoint.Role != "prefill" && endpoint.Role != "decode" {
		return nil, fmt.Errorf("role must be prefill or decode")
	}
	if endpoint.RoleSet != "" && strings.TrimSpace(endpoint.RoleSet) == "" {
		return nil, fmt.Errorf("roleset must not be blank")
	}
	host, portString, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host == "" {
		return nil, fmt.Errorf("address must be host:port or [IPv6]:port")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if len(validation.IsDNS1123Subdomain(host)) != 0 {
			return nil, fmt.Errorf("address host must be an IP address or DNS name")
		}
	}
	port, err := strconv.Atoi(portString)
	if err != nil || port < 1 || port > 65535 || strings.Trim(portString, "0123456789") != "" {
		return nil, fmt.Errorf("address port must be an integer between 1 and 65535")
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	labels := make(map[string]string)
	if endpoint.Role != "" {
		labels["role-name"] = endpoint.Role
		labels["roleset-name"] = endpoint.RoleSet
	}
	pod, err := addressToPod(endpoint.Model, endpoint.Engine, labels, 0, address)
	if err != nil {
		return nil, fmt.Errorf("cannot convert registered endpoint to a pod")
	}
	// Include every route identity field and the key, but exclude mutable engine
	// metadata. JSON encoding unambiguously separates fields even with NUL bytes.
	identity, _ := json.Marshal([]string{string(kv.Key), endpoint.Model, address, endpoint.Role, endpoint.RoleSet})
	digest := sha256.Sum256(identity)
	pod.Name = fmt.Sprintf("etcd-%x", digest[:20])
	pod.UID = types.UID(fmt.Sprintf("%s-%d", pod.Name, kv.CreateRevision))
	return pod, nil
}

var _ Provider = (*EtcdProvider)(nil)
