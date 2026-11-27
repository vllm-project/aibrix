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

package router

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
)

func TestNewHTTPTransport_Defaults(t *testing.T) {
	tr := pd.NewHTTPTransport()
	assert.Equal(t, pd.DefaultHTTPMaxIdleConns, tr.MaxIdleConns)
	assert.Equal(t, pd.DefaultHTTPMaxIdleConnsPerHost, tr.MaxIdleConnsPerHost)
	assert.Equal(t, pd.DefaultHTTPIdleConnTimeout, tr.IdleConnTimeout)
	assert.Equal(t, 1024, tr.MaxIdleConns)
	assert.Equal(t, 128, tr.MaxIdleConnsPerHost)
	assert.Equal(t, 90*time.Second, tr.IdleConnTimeout)

	rawRouter, err := NewPDRouterWithCacheAndPrefixIndexer(cache.NewForTest(), nil)
	require.NoError(t, err)
	r, ok := rawRouter.(*pdRouter)
	require.True(t, ok)
	require.NotNil(t, r.httpClient)
	assert.Zero(t, r.httpClient.Timeout, "PD router HTTP client must not impose a client-level timeout")
}

// TestPDHTTPTransport_ReusesConnectionsAboveOldPoolCaps sends two waves of 128
// simultaneous requests (4 servers x 32 per host) through the PD router's real
// HTTP client, holding each wave in flight together so all 128 connections must
// stay open concurrently. Because 32 > old MaxIdleConnsPerHost (10) and
// 128 > old MaxIdleConns (100), reverting either limit on pdRouter's transport
// forces wave 2 to dial new TCP connections and fails this test.
func TestPDHTTPTransport_ReusesConnectionsAboveOldPoolCaps(t *testing.T) {
	const (
		numHosts = 4
		perHost  = 32
		total    = numHosts * perHost
	)

	var (
		waveMu      sync.Mutex
		waveArrived int
		waveGate    = make(chan struct{})
		newTCPConns atomic.Int64
	)

	servers := make([]*httptest.Server, numHosts)
	for h := 0; h < numHosts; h++ {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()

			waveMu.Lock()
			waveArrived++
			gate := waveGate
			if waveArrived == total {
				close(gate)
			}
			waveMu.Unlock()

			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				newTCPConns.Add(1)
			}
		}
		srv.Start()
		servers[h] = srv
		t.Cleanup(srv.Close)
	}

	rawRouter, err := NewPDRouterWithCacheAndPrefixIndexer(cache.NewForTest(), nil)
	require.NoError(t, err)
	r := rawRouter.(*pdRouter)
	defer r.httpClient.CloseIdleConnections()

	runWave := func() (reused, fresh int64) {
		t.Helper()
		waveMu.Lock()
		waveArrived = 0
		waveGate = make(chan struct{})
		waveMu.Unlock()

		var reusedCnt, freshCnt atomic.Int64
		errCh := make(chan error, total)
		var wg sync.WaitGroup
		for h := 0; h < numHosts; h++ {
			targetURL := servers[h].URL + "/v1/chat/completions"
			for i := 0; i < perHost; i++ {
				wg.Add(1)
				go func(url string) {
					defer wg.Done()
					trace := &httptrace.ClientTrace{
						GotConn: func(info httptrace.GotConnInfo) {
							if info.Reused {
								reusedCnt.Add(1)
							} else {
								freshCnt.Add(1)
							}
						},
					}
					reqCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(context.Background(), trace), 10*time.Second)
					defer cancel()
					req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(`{"model":"m"}`))
					if reqErr != nil {
						errCh <- reqErr
						return
					}
					resp, doErr := r.httpClient.Do(req)
					if doErr != nil {
						errCh <- doErr
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}(targetURL)
			}
		}
		wg.Wait()
		close(errCh)
		for callErr := range errCh {
			require.NoError(t, callErr)
		}
		return reusedCnt.Load(), freshCnt.Load()
	}

	// Wave 1 opens 128 simultaneous TCP connections across 4 hosts and returns them to the idle pool.
	wave1Reused, wave1Fresh := runWave()
	assert.Equal(t, int64(0), wave1Reused)
	assert.Equal(t, int64(total), wave1Fresh)
	assert.Equal(t, int64(total), newTCPConns.Load())

	// Wave 2 needs all 128 connections again; every one must be reused with zero new TCP handshakes.
	wave2Reused, wave2Fresh := runWave()
	assert.Equal(t, int64(total), wave2Reused, "all %d connections from wave 1 must be reused", total)
	assert.Equal(t, int64(0), wave2Fresh, "wave 2 must not dial new TCP connections")
	assert.Equal(t, int64(total), newTCPConns.Load(), "servers must observe no additional TCP handshakes in wave 2")
}
