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

//go:build zmq

package kvevent

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache/kvcache"
	"github.com/vllm-project/aibrix/pkg/constants"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func updateTestPod() *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "relabeled", Namespace: "default", Labels: map[string]string{
		constants.ModelLabelName: "old-model", constants.LoraIDLabel: "11", constants.KVEventsEnabledLabel: "true",
	}}, Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "127.0.0.1",
		Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}}
}

func setPodReady(pod *v1.Pod, ready bool) {
	status := v1.ConditionFalse
	if ready {
		status = v1.ConditionTrue
	}
	pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: status}}
}

// countSubscriberStarts makes m start real clients, as the other tests here do
// (no publisher is needed), and counts the starts.
func countSubscriberStarts(m *Manager) *int {
	starts := 0
	m.startSubscriber = func(c *kvcache.ZMQClientConfig, h kvcache.EventHandler) (*kvcache.ZMQClient, error) {
		c.ReplayTimeout = time.Millisecond
		starts++
		return startZMQSubscriber(c, h)
	}
	return &starts
}

// testPodProvider serves pods to OnPodAdd as the store does.
type testPodProvider map[string]*v1.Pod

func (p testPodProvider) GetPod(_ context.Context, podKey string) (*PodInfo, bool) {
	pod, ok := p[podKey]
	if !ok {
		return nil, false
	}
	return podInfoFromPod(pod), true
}

func (p testPodProvider) RangePods(context.Context, func(string, *PodInfo) bool) error {
	return nil
}

func newUpdateTestManager(t *testing.T) (*Manager, *mockSyncIndexerWithErrors) {
	t.Helper()
	m, indexer := newBackstopTestManager()
	m.enabled = true
	m.ctx, m.cancel = context.WithCancel(context.Background())
	t.Cleanup(m.Stop)
	return m, indexer
}

func TestManagerPodUpdateSubscribableIdentityChange(t *testing.T) {
	for _, field := range []string{"model", "lora"} {
		t.Run(field, func(t *testing.T) {
			m, indexer := newUpdateTestManager(t)
			oldPod := updateTestPod()
			newPod := oldPod.DeepCopy()
			if field == "model" {
				newPod.Labels[constants.ModelLabelName] = "new-model"
			} else {
				newPod.Labels[constants.LoraIDLabel] = "22"
			}
			if !isPodSubscribable(oldPod) || !isPodSubscribable(newPod) || !isSamePod(oldPod, newPod) {
				t.Fatal("fixture must isolate identity change while both pods remain subscribable at the same IP")
			}
			var handlers []*eventHandler
			var configs []*kvcache.ZMQClientConfig
			m.startSubscriber = func(c *kvcache.ZMQClientConfig, h kvcache.EventHandler) (*kvcache.ZMQClient, error) {
				c.ReplayTimeout = time.Millisecond
				if len(configs) == 1 && !reflect.DeepEqual(indexer.removePrefixCalls, []removePrefixCall{{"old-model", 11, "default/relabeled"}}) {
					t.Fatal("old context must be purged exactly once before starting the new subscriber")
				}
				configs = append(configs, c)
				handlers = append(handlers, h.(*eventHandler))
				// Exercise the real client constructor, socket startup and shutdown. No
				// publisher is needed: ZMQ connects asynchronously to the loopback address.
				return startZMQSubscriber(c, h)
			}
			key := "default/relabeled"
			if err := m.subscribeToPod(m.ctx, key, podInfoFromPod(oldPod)); err != nil {
				t.Fatal(err)
			}
			oldClient, _ := m.subscribers.Load(key)
			t.Cleanup(oldClient.Stop)
			m.OnPodUpdate(oldPod, newPod)
			newClient, exists := m.subscribers.Load(key)
			if !exists || newClient == oldClient {
				t.Fatal("identity change must replace the old subscriber")
			}
			if oldClient.IsConnected() {
				t.Fatal("old subscriber was not stopped")
			}
			wantPurge := []removePrefixCall{{"old-model", 11, key}}
			if !reflect.DeepEqual(indexer.removePrefixCalls, wantPurge) {
				t.Fatalf("purges = %+v, want %+v", indexer.removePrefixCalls, wantPurge)
			}
			if len(handlers) != 2 {
				t.Fatalf("subscriber starts = %d, want 2", len(handlers))
			}
			wantModel, wantLora := podEventIdentity(newPod)
			if configs[1].ModelName != wantModel || configs[1].PodIP != newPod.Status.PodIP {
				t.Fatalf("new subscriber config = %+v", configs[1])
			}
			// Deliver through the handler actually passed to the new client, so missing
			// Labels in podInfoFromPod or reuse of oldPod changes the observed attribution.
			if err := handlers[1].HandleEvent(&kvcache.BlockStoredEvent{BlockHashes: []int64{42}, TokenIDs: [][]byte{{1}}}); err != nil {
				t.Fatal(err)
			}
			if len(indexer.storedEvents) != 1 {
				t.Fatalf("stored events = %d", len(indexer.storedEvents))
			}
			got := indexer.storedEvents[0]
			if got.ModelName != wantModel || got.LoraID != wantLora || got.SourcePod != key {
				t.Fatalf("new event attribution = %+v, want %s/%d/%s", got, wantModel, wantLora, key)
			}
		})
	}
}

func TestManagerPodUpdateSubscribeInterleavesStop(t *testing.T) {
	m, _ := newUpdateTestManager(t)
	oldPod := updateTestPod()
	newPod := oldPod.DeepCopy()
	newPod.Labels[constants.LoraIDLabel] = "22"
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var startedClient *kvcache.ZMQClient
	m.startSubscriber = func(c *kvcache.ZMQClientConfig, h kvcache.EventHandler) (*kvcache.ZMQClient, error) {
		c.ReplayTimeout = time.Millisecond
		var err error
		startedClient, err = startZMQSubscriber(c, h)
		if err != nil {
			return nil, err
		}
		// The real client is running, but the manager cannot see it until Store.
		close(entered)
		<-release
		return startedClient, nil
	}
	go func() { defer close(done); m.OnPodUpdate(oldPod, newPod) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("update did not reach subscriber startup")
	}
	// Stop must finish its Range before the new subscriber is stored.
	m.Stop()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("update did not finish after startup was released")
	}
	if _, exists := m.subscribers.Load("default/relabeled"); exists {
		t.Error("subscriber stored after Stop must be removed by the post-subscribe stopped check")
	}
	if startedClient.IsConnected() {
		startedClient.Stop()
		t.Fatal("subscriber stored after Stop must have its sockets closed, not only be removed from the map")
	}
}

// TestManagerSubscribesWhenPodTurnsReady checks that a pod is first subscribed
// when it turns Ready, not when it starts Running: vLLM binds its replay
// socket only after loading the model, while the pod is Running but not Ready.
func TestManagerSubscribesWhenPodTurnsReady(t *testing.T) {
	m, indexer := newUpdateTestManager(t)
	starts := countSubscriberStarts(m)
	key := "default/relabeled"

	pending := updateTestPod()
	pending.Status.Phase = v1.PodPending
	setPodReady(pending, false)
	loading := pending.DeepCopy()
	loading.Status.Phase = v1.PodRunning
	m.OnPodUpdate(pending, loading)
	if _, exists := m.subscribers.Load(key); exists || *starts != 0 {
		t.Fatalf("subscribed to a pod that is Running but not Ready (starts=%d)", *starts)
	}

	ready := loading.DeepCopy()
	setPodReady(ready, true)
	m.OnPodUpdate(loading, ready)
	client, exists := m.subscribers.Load(key)
	if !exists || *starts != 1 {
		t.Fatalf("pod turned Ready: subscribed=%v, starts=%d; want one subscription", exists, *starts)
	}

	// Losing readiness later (a slow probe under load) keeps the subscription
	// and the pod's prefixes; turning Ready again starts no second client.
	notReady := ready.DeepCopy()
	setPodReady(notReady, false)
	m.OnPodUpdate(ready, notReady)
	m.OnPodUpdate(notReady, ready)
	if now, _ := m.subscribers.Load(key); now != client || *starts != 1 || !client.IsConnected() {
		t.Fatalf("a NotReady flap replaced or stopped the subscription (starts=%d)", *starts)
	}
	if len(indexer.removePrefixCalls) != 0 {
		t.Fatalf("a NotReady flap purged the pod's prefixes: %+v", indexer.removePrefixCalls)
	}
}

func TestManagerPodAddSubscribesOnlyWhenReady(t *testing.T) {
	m, _ := newUpdateTestManager(t)
	starts := countSubscriberStarts(m)
	key := "default/relabeled"
	pod := updateTestPod()
	m.podProvider = testPodProvider{key: pod}

	setPodReady(pod, false)
	m.OnPodAdd(pod)
	if _, exists := m.subscribers.Load(key); exists || *starts != 0 {
		t.Fatalf("subscribed to an added pod that is not Ready (starts=%d)", *starts)
	}

	setPodReady(pod, true)
	m.OnPodAdd(pod)
	if _, exists := m.subscribers.Load(key); !exists || *starts != 1 {
		t.Fatalf("added Ready pod: subscribed=%v, starts=%d; want one subscription", exists, *starts)
	}
}

// TestManagerPodRelabeledWhileNotReadyWaitsForReady checks that a model change
// on a pod that is not Ready still ends the old subscription and purges the old
// context at once, and starts the new subscription when the pod is Ready.
func TestManagerPodRelabeledWhileNotReadyWaitsForReady(t *testing.T) {
	m, indexer := newUpdateTestManager(t)
	starts := countSubscriberStarts(m)
	key := "default/relabeled"
	oldPod := updateTestPod()
	if err := m.subscribeToPod(m.ctx, key, podInfoFromPod(oldPod)); err != nil {
		t.Fatal(err)
	}
	oldClient, _ := m.subscribers.Load(key)

	notReady := oldPod.DeepCopy()
	setPodReady(notReady, false)
	m.OnPodUpdate(oldPod, notReady)
	relabeled := notReady.DeepCopy()
	relabeled.Labels[constants.ModelLabelName] = "new-model"
	m.OnPodUpdate(notReady, relabeled)
	if _, exists := m.subscribers.Load(key); exists || *starts != 1 {
		t.Fatalf("relabeled pod that is not Ready: subscribed=%v, starts=%d; want the old client stopped and no new one", exists, *starts)
	}
	if oldClient.IsConnected() {
		t.Fatal("old subscriber was not stopped")
	}
	wantPurge := []removePrefixCall{{"old-model", 11, key}}
	if !reflect.DeepEqual(indexer.removePrefixCalls, wantPurge) {
		t.Fatalf("purges = %+v, want %+v", indexer.removePrefixCalls, wantPurge)
	}

	ready := relabeled.DeepCopy()
	setPodReady(ready, true)
	m.OnPodUpdate(relabeled, ready)
	client, exists := m.subscribers.Load(key)
	if !exists || *starts != 2 {
		t.Fatalf("relabeled pod turned Ready: subscribed=%v, starts=%d; want a new subscription", exists, *starts)
	}
	if client == oldClient {
		t.Fatal("the old subscriber was reused for the new model")
	}
	if !reflect.DeepEqual(indexer.removePrefixCalls, wantPurge) {
		t.Fatalf("purges = %+v, want only %+v", indexer.removePrefixCalls, wantPurge)
	}
}
