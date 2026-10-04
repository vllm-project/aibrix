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

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

type modelWakeCall struct {
	pod     *v1.Pod
	model   string
	binding utils.ModelClaimBinding
}

type recordingModelWakeRequester struct {
	calls []modelWakeCall
}

func (r *recordingModelWakeRequester) RequestWake(pod *v1.Pod, binding utils.ModelClaimBinding) bool {
	r.calls = append(r.calls, modelWakeCall{pod: pod, model: binding.Model, binding: binding})
	return true
}

func TestRuntimeModelWakeRequesterDeduplicatesAcrossPodResourceVersions(t *testing.T) {
	type wakePayload struct {
		ModelName   string `json:"model_name"`
		OperationID string `json:"operation_id"`
	}
	type wakeRequest struct {
		path    string
		payload wakePayload
		err     error
	}
	received := make(chan wakeRequest, 2)
	release := make(chan struct{})
	completed := make(chan struct{}, 2)
	var requestCount int
	var countMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() { completed <- struct{}{} }()
		var payload wakePayload
		decodeErr := json.NewDecoder(request.Body).Decode(&payload)
		countMu.Lock()
		requestCount++
		countMu.Unlock()
		received <- wakeRequest{path: request.URL.Path, payload: payload, err: decodeErr}
		<-release
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"success"}`))
	}))
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, rawPort, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(rawPort)
	require.NoError(t, err)
	requester := newRuntimeModelWakeRequester(server.Client(), port, nil)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warm-1", Namespace: "default", UID: types.UID("pod-uid"), ResourceVersion: "42",
		},
		Status: v1.PodStatus{PodIP: host},
	}

	assert.True(t, requester.RequestWake(pod, utils.ModelClaimBinding{Model: "qwen"}))
	updatedPod := pod.DeepCopy()
	updatedPod.ResourceVersion = "43"
	assert.False(t, requester.RequestWake(updatedPod, utils.ModelClaimBinding{Model: "qwen"}))
	call := <-received
	require.NoError(t, call.err)
	assert.Equal(t, "/v1/runtime/models/wake", call.path)
	assert.Equal(t, "qwen", call.payload.ModelName)
	assert.Contains(t, call.payload.OperationID, "pod-uid")
	close(release)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("wake request did not complete")
	}

	countMu.Lock()
	assert.Equal(t, 1, requestCount)
	countMu.Unlock()
}

func TestRuntimeModelWakeRequesterUsesUniqueOperationIDsAcrossAttempts(t *testing.T) {
	type wakePayload struct {
		ModelName   string `json:"model_name"`
		OperationID string `json:"operation_id"`
	}
	type wakeRequest struct {
		payload wakePayload
		err     error
	}
	received := make(chan wakeRequest, 2)
	completed := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() { completed <- struct{}{} }()
		var payload wakePayload
		decodeErr := json.NewDecoder(request.Body).Decode(&payload)
		received <- wakeRequest{payload: payload, err: decodeErr}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"success"}`))
	}))
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, rawPort, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(rawPort)
	require.NoError(t, err)
	requester := newRuntimeModelWakeRequester(server.Client(), port, nil)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warm-1", Namespace: "default", UID: types.UID("pod-uid"), ResourceVersion: "42",
		},
		Status: v1.PodStatus{PodIP: host},
	}

	assert.True(t, requester.RequestWake(pod, utils.ModelClaimBinding{Model: "qwen"}))
	first := <-received
	require.NoError(t, first.err)
	<-completed
	require.Eventually(t, func() bool {
		return requester.RequestWake(pod, utils.ModelClaimBinding{Model: "qwen"})
	}, time.Second, 10*time.Millisecond)
	second := <-received
	require.NoError(t, second.err)
	<-completed

	assert.Equal(t, "qwen", first.payload.ModelName)
	assert.Equal(t, "qwen", second.payload.ModelName)
	assert.NotEqual(t, first.payload.OperationID, second.payload.OperationID)
}

// A runtime that must not be called: its server fails the test on any request.
func noRuntime(t *testing.T) (*http.Client, int, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the runtime was called: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, rawPort, err := net.SplitHostPort(serverURL.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(rawPort)
	require.NoError(t, err)
	return server.Client(), port, host
}

func TestRuntimeModelWakeRequesterAsksTheControllerOnThePod(t *testing.T) {
	client, port, host := noRuntime(t)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-1", Namespace: "default", UID: types.UID("pod-uid")},
		Status:     v1.PodStatus{PodIP: host},
	}
	pods := fake.NewSimpleClientset(pod.DeepCopy())
	requester := newRuntimeModelWakeRequester(client, port, pods)
	asked := time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC)
	requester.now = func() time.Time { return asked }
	binding := utils.ModelClaimBinding{
		Model: "qwen", State: constants.ModelClaimRoutingStateSleeping, Claim: "qwen-claim", WakeByRequest: true,
	}

	assert.True(t, requester.RequestWake(pod, binding))

	key := constants.ModelClaimWakeAnnotationPrefix + "qwen-claim"
	require.Eventually(t, func() bool {
		got, err := pods.CoreV1().Pods("default").Get(context.Background(), "warm-1", metav1.GetOptions{})
		return err == nil && got.Annotations[key] == "2026-10-01T08:00:00Z"
	}, time.Second, 10*time.Millisecond)
	// The cache has not seen the request on the pod yet. It is not written
	// again so soon.
	require.Eventually(t, func() bool {
		_, running := requester.inFlight.Load("wake-request/pod-uid/default/warm-1/qwen-claim")
		return !running
	}, time.Second, 10*time.Millisecond)
	assert.False(t, requester.RequestWake(pod, binding))
	patches := 0
	for _, action := range pods.Actions() {
		if action.GetVerb() == "patch" {
			patches++
		}
	}
	assert.Equal(t, 1, patches)
}

// A request written on a pod keeps a second one from being written for a
// while. After that, it is forgotten, since a pod that is gone is never asked
// about again.
func TestRuntimeModelWakeRequesterForgetsOldWakeRequests(t *testing.T) {
	client, port, host := noRuntime(t)
	gone := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-1", Namespace: "default", UID: types.UID("gone-uid")},
		Status:     v1.PodStatus{PodIP: host},
	}
	next := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-2", Namespace: "default", UID: types.UID("next-uid")},
		Status:     v1.PodStatus{PodIP: host},
	}
	pods := fake.NewSimpleClientset(gone.DeepCopy(), next.DeepCopy())
	requester := newRuntimeModelWakeRequester(client, port, pods)
	now := time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC)
	requester.now = func() time.Time { return now }
	binding := utils.ModelClaimBinding{
		Model: "qwen", State: constants.ModelClaimRoutingStateSleeping, Claim: "qwen-claim", WakeByRequest: true,
	}
	written := func(key string) {
		t.Helper()
		require.Eventually(t, func() bool {
			_, running := requester.inFlight.Load(key)
			return !running
		}, time.Second, 10*time.Millisecond)
	}
	remembered := func() []string {
		var keys []string
		requester.asked.Range(func(key, _ any) bool {
			keys = append(keys, key.(string))
			return true
		})
		sort.Strings(keys)
		return keys
	}

	require.True(t, requester.RequestWake(gone, binding))
	written("wake-request/gone-uid/default/warm-1/qwen-claim")
	now = now.Add(10 * time.Second)
	require.True(t, requester.RequestWake(next, binding))
	written("wake-request/next-uid/default/warm-2/qwen-claim")
	assert.Equal(t, []string{
		"wake-request/gone-uid/default/warm-1/qwen-claim",
		"wake-request/next-uid/default/warm-2/qwen-claim",
	}, remembered())

	now = now.Add(modelClaimWakeRequestRecheck - 10*time.Second)
	assert.False(t, requester.RequestWake(next, binding))
	assert.Equal(t, []string{"wake-request/next-uid/default/warm-2/qwen-claim"}, remembered())
}

// The controller takes a request back after a wake that failed, or one that
// waited too long. The next request for the model writes a new one at once,
// whether or not the cache saw the first one on the pod.
func TestRuntimeModelWakeRequesterWritesAgainOnceTheControllerTookTheRequestBack(t *testing.T) {
	for name, seen := range map[string]bool{
		"the cache saw the request":       true,
		"the cache never saw the request": false,
	} {
		t.Run(name, func(t *testing.T) {
			client, port, host := noRuntime(t)
			key := constants.ModelClaimWakeAnnotationPrefix + "qwen-claim"
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "warm-1", Namespace: "default", UID: types.UID("pod-uid"), ResourceVersion: "10",
				},
				Status: v1.PodStatus{PodIP: host},
			}
			pods := fake.NewSimpleClientset(pod.DeepCopy())
			requester := newRuntimeModelWakeRequester(client, port, pods)
			asked := time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC)
			requester.now = func() time.Time { return asked }
			binding := utils.ModelClaimBinding{
				Model: "qwen", State: constants.ModelClaimRoutingStateSleeping, Claim: "qwen-claim", WakeByRequest: true,
			}
			written := func() {
				t.Helper()
				require.Eventually(t, func() bool {
					_, running := requester.inFlight.Load("wake-request/pod-uid/default/warm-1/qwen-claim")
					return !running
				}, time.Second, 10*time.Millisecond)
			}

			require.True(t, requester.RequestWake(pod, binding))
			written()
			asked = asked.Add(5 * time.Second)
			if seen {
				withRequest := pod.DeepCopy()
				withRequest.ResourceVersion = "11"
				withRequest.Annotations = map[string]string{key: "2026-10-01T08:00:00Z"}
				assert.False(t, requester.RequestWake(withRequest, binding), "the request is on the pod")
			}
			takenBack := pod.DeepCopy()
			takenBack.ResourceVersion = "12"

			require.True(t, requester.RequestWake(takenBack, binding))
			written()

			got, err := pods.CoreV1().Pods("default").Get(context.Background(), "warm-1", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, "2026-10-01T08:00:05Z", got.Annotations[key])
		})
	}
}

// A request is written only over the version of the pod whose route the
// gateway read. A pod that has changed since, as when the controller has taken
// the route away, refuses it. Nothing is remembered of a refused request, so
// the next request for the model reads the pod again.
func TestRuntimeModelWakeRequesterWritesOnlyOverThePodVersionItRead(t *testing.T) {
	client, port, host := noRuntime(t)
	key := constants.ModelClaimWakeAnnotationPrefix + "qwen-claim"
	read := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warm-1", Namespace: "default", UID: types.UID("pod-uid"), ResourceVersion: "10",
		},
		Status: v1.PodStatus{PodIP: host},
	}
	stored := read.DeepCopy()
	stored.ResourceVersion = "11"
	pods := fake.NewSimpleClientset(stored)
	// As the API server does, a patch that names a version other than the
	// pod's own is refused.
	var named []string
	pods.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		var body struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(action.(k8stesting.PatchAction).GetPatch(), &body); err != nil {
			return true, nil, err
		}
		named = append(named, body.Metadata.ResourceVersion)
		if version := body.Metadata.ResourceVersion; version != "" && version != "11" {
			return true, nil, apierrors.NewConflict(v1.Resource("pods"), "warm-1", errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	requester := newRuntimeModelWakeRequester(client, port, pods)
	requester.now = func() time.Time { return time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC) }
	binding := utils.ModelClaimBinding{
		Model: "qwen", State: constants.ModelClaimRoutingStateSleeping, Claim: "qwen-claim", WakeByRequest: true,
	}
	written := func() {
		t.Helper()
		require.Eventually(t, func() bool {
			_, running := requester.inFlight.Load("wake-request/pod-uid/default/warm-1/qwen-claim")
			return !running
		}, time.Second, 10*time.Millisecond)
	}
	annotation := func() (string, bool) {
		t.Helper()
		got, err := pods.CoreV1().Pods("default").Get(context.Background(), "warm-1", metav1.GetOptions{})
		require.NoError(t, err)
		value, found := got.Annotations[key]
		return value, found
	}

	require.True(t, requester.RequestWake(read, binding))
	written()
	_, found := annotation()
	assert.False(t, found, "a pod that has changed refuses the request")

	require.True(t, requester.RequestWake(stored.DeepCopy(), binding), "nothing was remembered of the refused request")
	written()
	value, found := annotation()
	assert.True(t, found)
	assert.Equal(t, "2026-10-01T08:00:00Z", value)
	assert.Equal(t, []string{"10", "11"}, named)
}

func TestRuntimeModelWakeRequesterDoesNotAskAgainWhileThePodCarriesTheRequest(t *testing.T) {
	client, port, host := noRuntime(t)
	key := constants.ModelClaimWakeAnnotationPrefix + "qwen-claim"
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warm-1", Namespace: "default", UID: types.UID("pod-uid"),
			Annotations: map[string]string{key: "2026-10-01T08:00:00Z"},
		},
		Status: v1.PodStatus{PodIP: host},
	}
	pods := fake.NewSimpleClientset(pod.DeepCopy())
	requester := newRuntimeModelWakeRequester(client, port, pods)

	assert.False(t, requester.RequestWake(pod, utils.ModelClaimBinding{
		Model: "qwen", State: constants.ModelClaimRoutingStateSleeping, Claim: "qwen-claim", WakeByRequest: true,
	}))
	assert.Empty(t, pods.Actions())
}

func TestRuntimeModelWakeRequesterWakesTheRuntimeWhenTheControllerDoesNot(t *testing.T) {
	for name, wakeByRequest := range map[string]bool{
		"the binding does not ask for it":         false,
		"the gateway runs without Kubernetes too": true,
	} {
		t.Run(name, func(t *testing.T) {
			received := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.URL.Path
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			serverURL, err := url.Parse(server.URL)
			require.NoError(t, err)
			host, rawPort, err := net.SplitHostPort(serverURL.Host)
			require.NoError(t, err)
			port, err := strconv.Atoi(rawPort)
			require.NoError(t, err)
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "warm-1", Namespace: "default", UID: types.UID("pod-uid")},
				Status:     v1.PodStatus{PodIP: host},
			}
			pods := fake.NewSimpleClientset(pod.DeepCopy())
			requester := newRuntimeModelWakeRequester(server.Client(), port, pods)
			if wakeByRequest {
				requester = newRuntimeModelWakeRequester(server.Client(), port, nil)
			}

			assert.True(t, requester.RequestWake(pod, utils.ModelClaimBinding{
				Model: "qwen", State: constants.ModelClaimRoutingStateSleeping, Claim: "qwen-claim",
				WakeByRequest: wakeByRequest,
			}))

			select {
			case path := <-received:
				assert.Equal(t, modelClaimWakePath, path)
			case <-time.After(time.Second):
				t.Fatal("the runtime was not asked to wake the engine")
			}
			assert.Empty(t, pods.Actions())
		})
	}
}
