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
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type modelWakeCall struct {
	pod   *v1.Pod
	model string
}

type recordingModelWakeRequester struct {
	calls []modelWakeCall
}

func (r *recordingModelWakeRequester) RequestWake(pod *v1.Pod, model string) bool {
	r.calls = append(r.calls, modelWakeCall{pod: pod, model: model})
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
	requester := newRuntimeModelWakeRequester(server.Client(), port)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warm-1", Namespace: "default", UID: types.UID("pod-uid"), ResourceVersion: "42",
		},
		Status: v1.PodStatus{PodIP: host},
	}

	assert.True(t, requester.RequestWake(pod, "qwen"))
	updatedPod := pod.DeepCopy()
	updatedPod.ResourceVersion = "43"
	assert.False(t, requester.RequestWake(updatedPod, "qwen"))
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
	requester := newRuntimeModelWakeRequester(server.Client(), port)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warm-1", Namespace: "default", UID: types.UID("pod-uid"), ResourceVersion: "42",
		},
		Status: v1.PodStatus{PodIP: host},
	}

	assert.True(t, requester.RequestWake(pod, "qwen"))
	first := <-received
	require.NoError(t, first.err)
	<-completed
	require.Eventually(t, func() bool {
		return requester.RequestWake(pod, "qwen")
	}, time.Second, 10*time.Millisecond)
	second := <-received
	require.NoError(t, second.err)
	<-completed

	assert.Equal(t, "qwen", first.payload.ModelName)
	assert.Equal(t, "qwen", second.payload.ModelName)
	assert.NotEqual(t, first.payload.OperationID, second.payload.OperationID)
}
