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

package routingalgorithms

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

func TestTokenizerPoolConcurrentCapacity(t *testing.T) {
	for _, sameModel := range []bool{false, true} {
		name := "different models"
		if sameModel {
			name = "same model"
		}
		t.Run(name, func(t *testing.T) {
			resetPrometheusRegistry()
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			closed := make(chan struct{}, 2)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case entered <- struct{}{}:
				case <-r.Context().Done():
					return
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = w.Write([]byte(`{"tokens":[],"count":0}`))
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					select {
					case closed <- struct{}{}:
					default:
					}
				}
			}
			server.Start()
			defer server.Close()

			fallback := tokenizer.NewCharacterTokenizer()
			// Keep the default 30s client timeout to avoid retries inside the barrier.
			pool := NewTokenizerPool(TokenizerPoolConfig{
				EnableVLLMRemote:     true,
				MaxTokenizersPerPool: 1,
				DefaultTokenizer:     fallback,
				ModelServiceMap:      map[string]string{"first": server.URL, "second": server.URL},
			}, nil)
			defer func() { require.NoError(t, pool.Close()) }()
			require.NotNil(t, pool.metrics)
			results := make([]tokenizer.Tokenizer, 2)
			finished := []chan struct{}{make(chan struct{}), make(chan struct{})}
			// Release handlers and join callers before closing the pool, even on failure.
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
				for _, done := range finished {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("tokenizer caller did not finish during cleanup")
					}
				}
			}()

			for i, model := range []string{"first", "second"} {
				if sameModel {
					model = "first"
				}
				go func() {
					results[i] = pool.GetTokenizer(model, nil)
					close(finished[i])
				}()
			}
			// Both calls must finish their initial capacity check before either
			// remote health check can succeed and commit a tokenizer to the pool.
			for range 2 {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("concurrent health check did not start")
				}
			}
			close(release)
			for _, done := range finished {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("tokenizer caller did not finish")
				}
			}
			// A health-check timeout must not masquerade as a capacity rejection.
			require.Zero(t, testutil.ToFloat64(pool.metrics.tokenizerCreationFailures))
			first, second := results[0], results[1]
			pool.mu.RLock()
			assert.Len(t, pool.tokenizers, 1)
			pool.mu.RUnlock()
			if sameModel {
				assert.Same(t, first, second)
				assert.NotSame(t, fallback, first)
			} else {
				assert.True(t, (first == fallback) != (second == fallback), "exactly one new model should use the fallback")
			}
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Error("discarded tokenizer did not close its idle connection")
			}
		})
	}
}

func TestTokenizerPoolReplaceUnhealthyAtCapacity(t *testing.T) {
	resetPrometheusRegistry()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tokens":[],"count":0}`))
	}))
	defer server.Close()
	fallback := tokenizer.NewCharacterTokenizer()
	pool := NewTokenizerPool(TokenizerPoolConfig{
		EnableVLLMRemote:     true,
		MaxTokenizersPerPool: 1,
		DefaultTokenizer:     fallback,
		ModelServiceMap:      map[string]string{"model": server.URL},
	}, nil)
	defer func() { require.NoError(t, pool.Close()) }()
	previous := &mockTokenizer{}
	previous.On("Close").Return(nil).Once()
	pool.mu.Lock()
	pool.tokenizers["model"] = &tokenizerEntry{tokenizer: previous, healthStatus: false}
	pool.mu.Unlock()

	result := pool.GetTokenizer("model", nil)
	assert.NotSame(t, fallback, result)
	assert.NotSame(t, previous, result)
	pool.mu.RLock()
	assert.Len(t, pool.tokenizers, 1)
	assert.Same(t, result, pool.tokenizers["model"].tokenizer)
	assert.True(t, pool.tokenizers["model"].healthStatus)
	pool.mu.RUnlock()
	previous.AssertExpectations(t)
}

func TestTokenizerPoolFailedHealthCheckReleasesClient(t *testing.T) {
	resetPrometheusRegistry()
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()
	fallback := tokenizer.NewCharacterTokenizer()
	pool := NewTokenizerPool(TokenizerPoolConfig{
		EnableVLLMRemote:     true,
		MaxTokenizersPerPool: 1,
		DefaultTokenizer:     fallback,
		ModelServiceMap:      map[string]string{"model": server.URL},
	}, nil)
	defer func() { require.NoError(t, pool.Close()) }()

	assert.Same(t, fallback, pool.GetTokenizer("model", nil))
	pool.mu.RLock()
	assert.Empty(t, pool.tokenizers)
	pool.mu.RUnlock()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("failed health check did not close the tokenizer's idle connection")
	}
}
