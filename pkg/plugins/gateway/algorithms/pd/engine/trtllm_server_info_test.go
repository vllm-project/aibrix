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

package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

const validTRTServerInfo = `{"disaggregated_params":{"ctx_info_endpoint":"tcp://10.0.0.1:5555","ctx_dp_rank":3,"encoded_opaque_state":"opaque"}}`

func trtInfoPod(t *testing.T, server *httptest.Server) *v1.Pod {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "ctx-uid", Labels: map[string]string{constants.ModelLabelPort: port}},
		Status:     v1.PodStatus{PodIP: host},
	}
}

func TestTRTServerInfoCacheLifecycle(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/server_info", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)
		calls.Add(1)
		_, _ = w.Write([]byte(validTRTServerInfo))
	}))
	defer srv.Close()
	pod := trtInfoPod(t, srv)
	cache := NewTRTServerInfoCache(srv.Client())
	now := time.Now()
	cache.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		info, err := cache.Get(context.Background(), pod)
		require.NoError(t, err)
		assert.Equal(t, 3, info.ContextDPRank, "never substitute replica index or rank zero")
		assert.Equal(t, "tcp://10.0.0.1:5555", info.ContextInfoEndpoint)
		assert.Equal(t, "opaque", info.EncodedOpaqueState)
	}
	assert.EqualValues(t, 1, calls.Load())

	pod.UID = "replacement"
	_, err := cache.Get(context.Background(), pod)
	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())

	pod.Status.ContainerStatuses = []v1.ContainerStatus{{Name: "engine", ContainerID: "container-2", RestartCount: 1}}
	_, err = cache.Get(context.Background(), pod)
	require.NoError(t, err)
	assert.EqualValues(t, 3, calls.Load())

	now = now.Add(trtServerInfoTTL)
	_, err = cache.Get(context.Background(), pod)
	require.NoError(t, err)
	assert.EqualValues(t, 4, calls.Load())
	assert.Len(t, cache.entries, 1, "refresh prunes expired incarnations")
}

func TestTRTServerInfoCacheCoalescesMisses(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			_, _ = w.Write([]byte(validTRTServerInfo))
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer unblock()
	cache := NewTRTServerInfoCache(srv.Client())
	pod := trtInfoPod(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaderDone := make(chan error, 1)
	go func() { _, err := cache.Get(ctx, pod); leaderDone <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("lookup did not start")
	}
	cancel()
	require.ErrorIs(t, <-leaderDone, context.Canceled)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := cache.Get(context.Background(), pod)
			assert.NoError(t, err)
			assert.Equal(t, 3, info.ContextDPRank)
		}()
	}
	unblock()
	wg.Wait()
	assert.EqualValues(t, 1, calls.Load(), "canceling one waiter must not poison other lookups")
}

func TestTRTServerInfoValidationAndRetry(t *testing.T) {
	for name, body := range map[string]string{
		"invalid JSON":        `not json`,
		"array":               `[]`,
		"missing params":      `{}`,
		"missing rank":        `{"disaggregated_params":{"ctx_info_endpoint":"tcp://host:1"}}`,
		"null rank":           `{"disaggregated_params":{"ctx_info_endpoint":"tcp://host:1","ctx_dp_rank":null}}`,
		"negative rank":       `{"disaggregated_params":{"ctx_info_endpoint":"tcp://host:1","ctx_dp_rank":-1}}`,
		"fractional rank":     `{"disaggregated_params":{"ctx_info_endpoint":"tcp://host:1","ctx_dp_rank":1.5}}`,
		"missing endpoint":    `{"disaggregated_params":{"ctx_dp_rank":0}}`,
		"wrong endpoint type": `{"disaggregated_params":{"ctx_dp_rank":0,"ctx_info_endpoint":["x"]}}`,
		"oversized":           strings.Repeat(" ", trtServerInfoMaxBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					_, _ = w.Write([]byte(body))
					return
				}
				_, _ = w.Write([]byte(validTRTServerInfo))
			}))
			defer srv.Close()
			cache := NewTRTServerInfoCache(srv.Client())
			pod := trtInfoPod(t, srv)
			_, err := cache.Get(context.Background(), pod)
			require.Error(t, err)
			assert.Empty(t, cache.entries)
			_, err = cache.Get(context.Background(), pod)
			require.NoError(t, err, "failed lookups must not be cached")
		})
	}
}

func TestTRTServerInfoHTTPFailures(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/elsewhere")
				w.WriteHeader(code)
			}))
			defer srv.Close()
			_, err := NewTRTServerInfoCache(srv.Client()).Get(context.Background(), trtInfoPod(t, srv))
			require.ErrorContains(t, err, fmt.Sprintf("HTTP %d", code))
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestTRTServerInfoCacheBoundsAndWorkerIsolation(t *testing.T) {
	cache := NewTRTServerInfoCache(nil)
	for i := 0; i < trtServerInfoMaxEntries+5; i++ {
		cache.store(fmt.Sprint(i), TRTServerInfo{ContextDPRank: i})
	}
	assert.Len(t, cache.entries, trtServerInfoMaxEntries)

	servers := make([]*httptest.Server, 2)
	for i := range servers {
		rank := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"disaggregated_params":{"ctx_info_endpoint":"tcp://worker-%d:123","ctx_dp_rank":%d}}`, rank, rank)
		}))
		defer servers[i].Close()
	}
	for i, srv := range servers {
		pod := trtInfoPod(t, srv)
		pod.UID = k8stypes.UID(fmt.Sprint(i))
		info, err := cache.Get(context.Background(), pod)
		require.NoError(t, err)
		assert.Equal(t, i, info.ContextDPRank)
		assert.Equal(t, fmt.Sprintf("tcp://worker-%d:123", i), info.ContextInfoEndpoint)
	}
}
