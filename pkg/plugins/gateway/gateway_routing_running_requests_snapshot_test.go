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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	pdrouter "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/router"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/prefixcache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// TestSelectTargetPod_ReadsRunningRequestsOnce routes one request per strategy over n ready
// pods against a Redis protocol server and counts the commands the routing path sends. The
// inflight filter, the load-imbalance gate, the strategy and its auto-blend all read the same
// pods' running counts; one routing decision must read them from Redis once (n HGETALLs plus
// the live-gateways read), not once per reader.
func TestSelectTargetPod_ReadsRunningRequestsOnce(t *testing.T) {
	const n = 100
	mr := miniredis.RunT(t)
	reads := &fleetReadCounter{}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%03d", i), Namespace: "ns"},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.%d.%d", i/256, i%256),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	store := cache.InitWithPods(cache.New(client, nil, nil), pods, "m")
	t.Cleanup(store.Close)
	rm := routing.NewRouterManagerWithCache(store)
	rm.Init()
	s := &Server{cache: store, routerManager: rm}

	// One fleet read is n HGETALLs and one ZRANGEBYSCORE of the live gateways, in one pipeline.
	const oneRead = n + 1
	strategies := []types.RoutingAlgorithm{
		least.RouterLeastRequest,
		routing.RouterLoadBalance,
		routing.RouterThroughput,
		least.RouterLeastLatency,
		least.RouterLeastKvCache,
		prefixcache.RouterPrefixCache,
		routing.RouterPowerOfTwo,
		routing.RouterRandom,
	}
	// Opt out of the default candidate stage (K=64) so every strategy reads the full 100-pod fleet once.
	noCandidateRaw := json.RawMessage(`{"candidate":{"k":0}}`)
	noCandidateRouting := configprofiles.ParseRoutingConfig(noCandidateRaw)
	for _, inflight := range []int64{0, 1000} {
		for _, strategy := range strategies {
			t.Run(fmt.Sprintf("%s/inflight=%d", strategy, inflight), func(t *testing.T) {
				ctx := types.NewRoutingContext(context.Background(), strategy, "m", "hello world", fmt.Sprintf("req-%s-%d", strategy, inflight), "")
				t.Cleanup(ctx.Delete)
				ctx.ConfigProfile = &types.ResolvedConfigProfile{
					RequestsInflight: inflight,
					RoutingConfig:    noCandidateRaw,
					Routing:          noCandidateRouting,
				}
				before, readsBefore := mr.CommandCount(), reads.count.Load()
				addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
				require.NoError(t, err)
				require.NotEmpty(t, addr)
				sent := mr.CommandCount() - before
				t.Logf("%d Redis commands for one request over %d pods", sent, n)
				// Every read of running-request counts, of the fleet or of any subset (e.g.
				// power-of-two's two pods), sends one ZRANGEBYSCORE of the live gateways.
				assert.EqualValues(t, 1, reads.count.Load()-readsBefore, "one read of running-request counts per decision")
				// A few commands besides the read are allowed (e.g. the admission under an
				// inflight cap); a second fleet read is not.
				assert.GreaterOrEqual(t, sent, oneRead, "the ready pods are read")
				assert.Less(t, sent, oneRead+n/2, "the ready pods are read once, not once per reader")

				// The decision's counts end with it: a read after selectTargetPod is live.
				readsBefore = reads.count.Load()
				_, err = ctx.RunningRequests(pods[:2], store.GetPodsRunningRequests)
				require.NoError(t, err)
				assert.EqualValues(t, 1, reads.count.Load()-readsBefore, "a read after the decision goes to Redis")
			})
		}
	}
}

// TestSelectTargetPod_PDReadsRunningRequestsOnce verifies that PD routing
// shares the per-decision RoutingContext.RunningRequests snapshot with
// narrowToInflightCapacity, sending one Redis read (ZRANGEBYSCORE + HGETALL
// pipeline) per routing decision rather than reading the decode pods a second
// time inside pdRouter.
func TestSelectTargetPod_PDReadsRunningRequestsOnce(t *testing.T) {
	const half = 50
	mr := miniredis.RunT(t)
	reads := &fleetReadCounter{}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(prefillServer.Close)
	u, err := url.Parse(prefillServer.URL)
	require.NoError(t, err)
	host, port := u.Hostname(), u.Port()

	pods := make([]*v1.Pod, 0, half*2)
	for i := 0; i < half; i++ {
		rs := fmt.Sprintf("rs-%d", i/5)
		pods = append(pods, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("prefill-%02d", i),
				Namespace: "ns",
				Labels: map[string]string{
					pdrouter.LLMEngineIdentifier: pdrouter.VLLMEngine,
					pdrouter.PDRoleIdentifier:    pdrouter.PDRolePrefill,
					pdrouter.PDRoleSetIdentifier: rs,
					constants.ModelLabelPort:     port,
				},
			},
			Status: v1.PodStatus{
				PodIP:      host,
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		})
		pods = append(pods, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("decode-%02d", i),
				Namespace: "ns",
				Labels: map[string]string{
					pdrouter.LLMEngineIdentifier: pdrouter.VLLMEngine,
					pdrouter.PDRoleIdentifier:    pdrouter.PDRoleDecode,
					pdrouter.PDRoleSetIdentifier: rs,
					constants.ModelLabelPort:     port,
				},
			},
			Status: v1.PodStatus{
				PodIP:      host,
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		})
	}
	store := cache.InitWithPods(cache.New(client, nil, nil), pods, "pd-m")
	t.Cleanup(store.Close)
	rm := routing.NewRouterManagerWithCache(store)
	rm.Init()
	s := &Server{cache: store, routerManager: rm}

	for _, inflight := range []int64{0, 1000} {
		t.Run(fmt.Sprintf("pd/inflight=%d", inflight), func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, "pd-m", "hello world", fmt.Sprintf("req-pd-%d", inflight), "user")
			t.Cleanup(ctx.Delete)
			ctx.ReqPath = "/v1/chat/completions"
			ctx.ReqBody = []byte(`{"model":"pd-m","messages":[{"role":"user","content":"hello world"}],"stream":true}`)
			ctx.ConfigProfile = &types.ResolvedConfigProfile{RequestsInflight: inflight}

			before, readsBefore := mr.CommandCount(), reads.count.Load()
			addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
			require.NoError(t, err)
			require.NotEmpty(t, addr)
			sent := mr.CommandCount() - before
			t.Logf("pd inflight=%d: %d Redis commands, %d fleet reads over %dP+%dD pods", inflight, sent, reads.count.Load()-readsBefore, half, half)

			assert.EqualValues(t, 1, reads.count.Load()-readsBefore, "PD routing must send one running-requests read per decision")
			if inflight == 0 {
				// Decode pods only (50 HGETALLs + 1 ZRANGEBYSCORE).
				assert.GreaterOrEqual(t, sent, half+1)
				assert.Less(t, sent, half+20)
			} else {
				// One read of decode pods only (50 HGETALLs + 1 ZRANGEBYSCORE) plus admission:
				// prefill pods are excluded from the inflight Redis check (#3619) and pdRouter
				// reuses the decode snapshot instead of reading decode pods a second time.
				assert.GreaterOrEqual(t, sent, half+1)
				assert.Less(t, sent, half+25)
			}

			// The snapshot ends with selectTargetPod: a subsequent read goes live to Redis.
			readsBefore = reads.count.Load()
			_, err = ctx.RunningRequests(pods[:2], store.GetPodsRunningRequests)
			require.NoError(t, err)
			assert.EqualValues(t, 1, reads.count.Load()-readsBefore, "a read after the decision goes to Redis")
		})
	}

	// Under token_load + requestsInflight, narrowPDToInflightCapacity populates the
	// decision-level running-requests snapshot for decode pods only (50 aibrix:rrq:*
	// HGETALLs, skipping prefill pods per #3619), and PD's decodeRunningRequests
	// reuses those cached counts, issuing only a ledger-only read (50 aibrix:dtl:*
	// HGETALLs, 0 duplicate aibrix:rrq:* reads). ZRANGEBYSCORE stays at 2 (one for
	// inflight, one for the ledger read to filter live gateways), keeping
	// gateway_inflight.go and pkg/types decoupled from PD/Redis internals.
	for _, inflight := range []int64{0, 1000} {
		t.Run(fmt.Sprintf("pd/token_load/inflight=%d", inflight), func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, "pd-m", "hello world", fmt.Sprintf("req-pd-token-load-%d", inflight), "user")
			t.Cleanup(ctx.Delete)
			ctx.ReqPath = "/v1/chat/completions"
			ctx.ReqBody = []byte(`{"model":"pd-m","messages":[{"role":"user","content":"hello world"}],"stream":true}`)
			raw := json.RawMessage(`{"decodeScorePolicy":"token_load"}`)
			ctx.ConfigProfile = &types.ResolvedConfigProfile{
				RequestsInflight: inflight,
				RoutingConfig:    raw,
				Routing:          configprofiles.ParseRoutingConfig(raw),
			}

			before := mr.CommandCount()
			readsBefore := reads.count.Load()
			rrqBefore := reads.rrqHGetAlls.Load()
			dtlBefore := reads.ledgerHGetAlls.Load()

			addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
			require.NoError(t, err)
			require.NotEmpty(t, addr)

			sent := mr.CommandCount() - before
			zrangeSent := reads.count.Load() - readsBefore
			rrqSent := reads.rrqHGetAlls.Load() - rrqBefore
			dtlSent := reads.ledgerHGetAlls.Load() - dtlBefore
			t.Logf("pd token_load inflight=%d: %d Redis commands, %d ZRANGEBYSCORE, %d rrq HGETALLs, %d dtl HGETALLs over %dP+%dD pods",
				inflight, sent, zrangeSent, rrqSent, dtlSent, half, half)

			if inflight == 0 {
				// Without a snapshot, token_load reads running counts and decode ledgers together in one round trip.
				assert.EqualValues(t, 1, zrangeSent, "token_load without inflight must use one combined Redis pipeline")
				assert.EqualValues(t, half, rrqSent, "token_load without inflight reads rrq only for decode pods")
				assert.EqualValues(t, half, dtlSent, "token_load without inflight reads dtl only for decode pods")
			} else {
				// With inflight enabled, narrowPDToInflightCapacity reads rrq once for the 50 decode pods only;
				// PD reuses that snapshot and reads only the 50 decode-ledger hashes (no prefill or duplicate rrq HGETALLs).
				assert.EqualValues(t, 2, zrangeSent, "token_load + requestsInflight sends inflight + ledger-only pipelines")
				assert.EqualValues(t, half, rrqSent, "token_load + requestsInflight reads aibrix:rrq:* only for decode pods")
				assert.EqualValues(t, half, dtlSent, "token_load + requestsInflight reads aibrix:dtl:* only for decode pods")
				assert.GreaterOrEqual(t, sent, half*2+2)
				assert.Less(t, sent, half*2+30)
			}
		})
	}
}

// fleetReadCounter counts ZRANGEBYSCORE and per-prefix HGETALL commands on
// connections opened by the copied dialer, including those owned by the store's
// derived Redis clients.
type fleetReadCounter struct {
	count          atomic.Int64
	rrqHGetAlls    atomic.Int64
	ledgerHGetAlls atomic.Int64
}

func (c *fleetReadCounter) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &fleetReadConn{Conn: conn, reads: c}, nil
}

type fleetReadConn struct {
	net.Conn
	reads   *fleetReadCounter
	pending []byte
}

func (c *fleetReadConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.pending = append(c.pending, p[:n]...)
	// A command may span socket writes; retain incomplete RESP frames.
	for len(c.pending) > 0 {
		input := bytes.NewReader(c.pending)
		reader := bufio.NewReader(input)
		command, parseErr := readRESPCommand(reader)
		if parseErr != nil {
			break
		}
		consumed := len(c.pending) - input.Len() - reader.Buffered()
		c.pending = c.pending[consumed:]
		if len(command) == 0 {
			continue
		}
		switch {
		case strings.EqualFold(command[0], "zrangebyscore"):
			c.reads.count.Add(1)
		case strings.EqualFold(command[0], "hgetall") && len(command) > 1:
			switch {
			case strings.HasPrefix(command[1], "aibrix:rrq:"):
				c.reads.rrqHGetAlls.Add(1)
			case strings.HasPrefix(command[1], "aibrix:dtl:"):
				c.reads.ledgerHGetAlls.Add(1)
			}
		}
	}
	return n, err
}

// readRESPCommand reads one RESP array of bulk strings, which is how a client
// sends a command. Inline commands are not supported: go-redis never sends them.
func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	header = strings.TrimRight(header, "\r\n")
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("unsupported RESP frame %q", header)
	}
	count, err := strconv.Atoi(strings.TrimPrefix(header, "*"))
	if err != nil {
		return nil, err
	}

	command := make([]string, 0, count)
	for i := 0; i < count; i++ {
		sizeLine, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimRight(strings.TrimPrefix(sizeLine, "$"), "\r\n"))
		if err != nil {
			return nil, err
		}
		payload := make([]byte, size+2) // +2 for the trailing CRLF
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, err
		}
		command = append(command, string(payload[:size]))
	}
	return command, nil
}
