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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
)

// Exercise each production entry point: blended routing uses ScoreAll, and the
// gateway's single-backend fast path uses PostRouteUpdate rather than Route.
var prefixContextOperations = []struct {
	name string
	call func(*prefixCacheRouter, *types.RoutingContext, types.PodList) error
}{
	{"route", func(r *prefixCacheRouter, ctx *types.RoutingContext, pods types.PodList) error {
		_, err := r.Route(ctx, pods)
		return err
	}},
	{"score", func(r *prefixCacheRouter, ctx *types.RoutingContext, pods types.PodList) error {
		_, _, err := r.ScoreAll(ctx, pods)
		return err
	}},
	{"post-route", func(r *prefixCacheRouter, ctx *types.RoutingContext, pods types.PodList) error {
		return r.PostRouteUpdate(ctx, pods, pods.All()[0])
	}},
}

func newPrefixContextRouter(t *testing.T, tok tokenizer.Tokenizer, kvSync bool) (*prefixCacheRouter, types.PodList) {
	t.Helper()
	pods := []*v1.Pod{
		newPod("context-pod-1", "10.0.0.1", true, map[string]string{constants.ModelLabelName: "test-model", constants.ModelLabelPort: "8000"}),
		newPod("context-pod-2", "10.0.0.2", true, map[string]string{constants.ModelLabelName: "test-model", constants.ModelLabelPort: "8000"}),
	}
	c := cache.NewWithPodsMetricsForTest(pods, "test-model", map[string]map[string]metrics.MetricValue{})
	t.Cleanup(c.Close)
	r := &prefixCacheRouter{
		cache: c, tokenizer: tok, tokenizerPool: &mockTokenizerPool{tokenizer: tok},
	}
	if kvSync {
		r.kvSyncRouter = &kvSyncPrefixCacheRouter{
			cache: c, tokenizerPool: &mockTokenizerPool{tokenizer: tok},
			syncIndexer: syncindexer.NewSyncPrefixHashTable(),
		}
		t.Cleanup(r.kvSyncRouter.syncIndexer.Close)
	} else {
		r.prefixCacheIndexer = prefixcacheindexer.NewPrefixHashTable()
	}
	return r, &utils.PodArray{Pods: pods}
}

func TestPrefixCacheRemoteTokenizationCanceledInFlight(t *testing.T) {
	for _, kvSync := range []bool{false, true} {
		mode := "local-index"
		if kvSync {
			mode = "kv-sync"
		}
		for _, operation := range prefixContextOperations {
			t.Run(mode+"/"+operation.name, func(t *testing.T) {
				arrived, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var arrivedOnce, canceledOnce sync.Once
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					arrivedOnce.Do(func() { close(arrived) })
					select {
					case <-r.Context().Done():
						canceledOnce.Do(func() { close(canceled) })
					case <-release:
						_, _ = io.WriteString(w, `{"tokens":[1,2],"count":2}`)
					}
				}))
				tok, err := tokenizer.NewRemoteTokenizer(tokenizer.RemoteTokenizerConfig{
					Engine: "vllm", Endpoint: server.URL, Model: "test-model", Timeout: 10 * time.Second, MaxRetries: 2,
				})
				require.NoError(t, err)
				router, pods := newPrefixContextRouter(t, tok, kvSync)
				parent, cancel := context.WithCancel(context.Background())
				ctx := types.NewRoutingContext(parent, RouterPrefixCache, "test-model", "hello", "context-request", "")
				result, finished := make(chan error, 1), make(chan struct{})
				go func() {
					defer close(finished)
					result <- operation.call(router, ctx, pods)
				}()
				t.Cleanup(func() {
					cancel()
					close(release)
					<-finished
					server.Close()
					_ = tok.(interface{ Close() error }).Close()
					ctx.Delete()
				})

				select {
				case <-arrived:
				case <-time.After(5 * time.Second):
					t.Fatal("tokenizer request did not arrive")
				}
				// Cancel only after the real transport has issued its request. No
				// tokenizer timeout or retry delay is used to order this test.
				cancel()
				select {
				case err := <-result:
					assert.ErrorIs(t, err, context.Canceled)
				case <-time.After(time.Second):
					t.Error("routing stayed blocked after the parent request was canceled")
				}
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Error("remote tokenizer did not observe request cancellation")
				}
				assert.EqualValues(t, 1, calls.Load(), "cancellation must not trigger another remote attempt")
			})
		}
	}
}

func TestPrefixCacheRemoteTokenizationAlreadyDone(t *testing.T) {
	for _, kvSync := range []bool{false, true} {
		for _, operation := range prefixContextOperations {
			for _, deadline := range []bool{false, true} {
				t.Run(operation.name+"/"+map[bool]string{false: "local-index", true: "kv-sync"}[kvSync]+"/"+map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						_, _ = io.WriteString(w, `{"tokens":[1,2],"count":2}`)
					}))
					defer server.Close()
					tok, err := tokenizer.NewRemoteTokenizer(tokenizer.RemoteTokenizerConfig{Engine: "vllm", Endpoint: server.URL})
					require.NoError(t, err)
					defer func() { _ = tok.(interface{ Close() error }).Close() }()
					router, pods := newPrefixContextRouter(t, tok, kvSync)
					parent, cancel := context.WithCancel(context.Background())
					cancel()
					if deadline {
						parent, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
						defer cancel()
					}
					ctx := types.NewRoutingContext(parent, RouterPrefixCache, "test-model", "hello", "context-request", "")
					defer ctx.Delete()
					assert.ErrorIs(t, operation.call(router, ctx, pods), parent.Err())
					assert.Zero(t, calls.Load(), "a finished request must not start remote tokenization")
				})
			}
		}
	}
}

func TestPrefixCacheRemoteTokenizationPreservesOptions(t *testing.T) {
	for _, specialTokens := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-special-tokens", true: "special-tokens"}[specialTokens], func(t *testing.T) {
			requests := make(chan map[string]any, 6)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body
				_, _ = io.WriteString(w, `{"tokens":[1,2],"count":2}`)
			}))
			defer server.Close()
			tok, err := tokenizer.NewRemoteTokenizer(tokenizer.RemoteTokenizerConfig{
				Engine: "vllm", Endpoint: server.URL, Model: "test-model", AddSpecialTokens: specialTokens,
			})
			require.NoError(t, err)
			defer func() { _ = tok.(interface{ Close() error }).Close() }()
			for _, kvSync := range []bool{false, true} {
				router, pods := newPrefixContextRouter(t, tok, kvSync)
				for _, operation := range prefixContextOperations {
					ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "test-model", "hello", "context-request", "")
					err := operation.call(router, ctx, pods)
					ctx.Delete()
					require.NoError(t, err)
					var body map[string]any
					select {
					case body = <-requests:
					case <-time.After(time.Second):
						t.Fatal("successful tokenization did not dispatch a request")
					}
					assert.Equal(t, specialTokens, body["add_special_tokens"])
					assert.Equal(t, "hello", body["prompt"])
					assert.Equal(t, "test-model", body["model"])
				}
			}
		})
	}
}

type prefixChatFallbackTokenizer struct {
	chatErr   error
	cancel    context.CancelFunc
	textCalls int
	chatCalls int
}

func (t *prefixChatFallbackTokenizer) TokenizeInputText(string) ([]byte, error) {
	t.textCalls++
	return tokenizer.IntToByteArray([]int{1, 2}), nil
}

func (t *prefixChatFallbackTokenizer) TokenizeWithOptions(context.Context, tokenizer.TokenizeInput) (*tokenizer.TokenizeResult, error) {
	t.chatCalls++
	if t.cancel != nil {
		t.cancel()
	}
	if t.chatErr != nil {
		return nil, t.chatErr
	}
	return &tokenizer.TokenizeResult{Tokens: []int{1, 2}}, nil
}

func (t *prefixChatFallbackTokenizer) Detokenize(context.Context, []int) (string, error) {
	return "", nil
}

func TestPrefixCacheChatFallbackHonorsParentContext(t *testing.T) {
	for _, tc := range []struct {
		name       string
		chatErr    error
		cancel     bool
		wantText   int
		wantCancel bool
	}{
		{"success", nil, false, 0, false},
		{"ordinary-error", errors.New("unsupported chat template"), false, 1, false},
		{"independent-timeout", context.DeadlineExceeded, false, 1, false},
		{"parent-canceled", context.Canceled, true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			tok := &prefixChatFallbackTokenizer{chatErr: tc.chatErr}
			if tc.cancel {
				tok.cancel = cancel
			}
			router, pods := newPrefixContextRouter(t, tok, true)
			ctx := types.NewRoutingContext(parent, RouterPrefixCache, "test-model", "hello", "chat-context-request", "")
			defer ctx.Delete()
			ctx.ReqPath = "/v1/chat/completions"
			ctx.ReqBody = []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`)
			_, err := router.Route(ctx, pods)
			if tc.wantCancel {
				assert.ErrorIs(t, err, context.Canceled)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, 1, tok.chatCalls)
			assert.Equal(t, tc.wantText, tok.textCalls)
		})
	}
}

// A local tokenizer implements only the original interface. A zero Context in a
// legacy RoutingContext must retain its existing text-tokenization behavior.
func TestPrefixCacheLocalTokenizationCompatibility(t *testing.T) {
	for _, kvSync := range []bool{false, true} {
		router, pods := newPrefixContextRouter(t, tokenizer.NewCharacterTokenizer(), kvSync)
		for _, operation := range prefixContextOperations {
			for _, noContext := range []bool{false, true} {
				ctx := types.NewRoutingContext(context.Background(), RouterPrefixCache, "test-model", "hello", "local-context-request", "")
				if noContext {
					ctx.Context = nil
				}
				assert.NoError(t, operation.call(router, ctx, pods))
				ctx.Delete()
			}
		}
	}
}
