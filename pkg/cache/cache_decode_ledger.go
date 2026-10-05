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

package cache

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// The token_load decode ledger is kept in each gateway process. These helpers
// share it across gateway replicas through Redis, next to the cross-gateway
// running-request counters (cache_running_requests.go), and reuse their
// instance ID, liveness set and field pruning.
//
// Each decode pod has one hash, aibrix:dtl:<ns>/<pod>, with one field per
// gateway instance (its running-requests instance ID) whose value packs that
// gateway's ledger for the pod as "<tokens>,<charges>,<sumat>":
//
//	tokens   prompt tokens charged and not yet released
//	charges  outstanding decode charges
//	sumat    sum of their charge times, Unix seconds on the Redis clock
//
// Unlike the running-request counters, a ledger field is only ever written by
// the gateway that owns it, so the gateway writes its absolute values instead
// of deltas: a lost or reordered write is corrected by the next one, and there
// is no increment to pair with a decrement.
const (
	decodeLedgerKeyPrefix = "aibrix:dtl"
	// decodeLedgerRepublishInterval is how often every pod with a nonzero ledger
	// is written again even without a change: it refreshes the hash TTLs and
	// repairs a write that failed.
	decodeLedgerRepublishInterval = time.Second
)

// DecodeLedgerState is one gateway's decode ledger for one pod.
type DecodeLedgerState struct {
	// Tokens is the prompt tokens charged to the pod and not yet released.
	Tokens float64
	// Charges is the number of outstanding decode charges.
	Charges int64
	// SumChargedAt is the sum of the outstanding charges' times, as Unix seconds
	// on this process's clock.
	SumChargedAt float64
}

func (s DecodeLedgerState) isZero() bool {
	return s.Tokens == 0 && s.Charges == 0
}

// RemoteDecodeLoad is what the other live gateway replicas hold on a pod.
type RemoteDecodeLoad struct {
	// Tokens is their charged prompt tokens.
	Tokens float64
	// Elapsed is the summed age of their outstanding charges, in seconds.
	Elapsed float64
}

// DecodeLedgerProvider returns this gateway's current decode ledger for the pod
// identified by podKey (namespace/name).
type DecodeLedgerProvider func(podKey string) DecodeLedgerState

func decodeLedgerKey(podKey string) string {
	return decodeLedgerKeyPrefix + ":" + podKey
}

// formatLedgerValue packs one gateway's ledger for a pod into its hash field.
func formatLedgerValue(tokens float64, charges int64, sumAt float64) string {
	return formatLedgerFloat(tokens) + "," + strconv.FormatInt(charges, 10) + "," + formatLedgerFloat(sumAt)
}

// parseLedgerValue unpacks a hash field written by formatLedgerValue.
func parseLedgerValue(v string) (tokens float64, charges int64, sumAt float64, ok bool) {
	parts := strings.Split(v, ",")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	tokens, err1 := strconv.ParseFloat(parts[0], 64)
	charges, err2 := strconv.ParseInt(parts[1], 10, 64)
	sumAt, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return tokens, charges, sumAt, true
}

// SharedDecodeLedgerAvailable reports whether this cache can share the decode
// ledger across gateway replicas, i.e. whether Redis is configured.
func (c *Store) SharedDecodeLedgerAvailable() bool {
	return c.redisClient != nil
}

// PublishDecodeLedger starts publishing the ledger that provider reads to Redis
// and returns the function to call with a pod key whenever that pod's ledger
// changes. The writes are coalesced per pod and made off the caller's path.
// Without Redis it returns a no-op.
//
// A later call replaces the earlier registration, whose returned function then
// does nothing. The gateway constructs its routers more than once at startup
// and serves with the last one constructed, so the last registration is the
// one whose ledger counts.
func (c *Store) PublishDecodeLedger(provider DecodeLedgerProvider) func(podKey string) {
	if c.redisClient == nil || provider == nil {
		return func(string) {}
	}
	p := newDecodeLedgerPublisher(c, provider)
	if old := c.decodeLedger.Swap(p); old != nil {
		klog.V(2).Info("decode ledger registered again; the later registration replaces the earlier one")
		old.close()
	}
	p.start()
	return p.markDirty
}

// decodeLedgerPublisher writes this gateway's decode ledger to Redis from one
// goroutine, so the writes for a pod reach Redis in the order they were made.
type decodeLedgerPublisher struct {
	store    *Store
	provider DecodeLedgerProvider

	mu sync.Mutex
	// dirty is the pods whose ledger changed since the last flush.
	dirty map[string]struct{}
	// published is the pods last written with a nonzero ledger: republished
	// every decodeLedgerRepublishInterval, and cleared once written as zero.
	published map[string]struct{}

	wake      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	// stopped is closed when run returns; nil until run starts.
	stopped chan struct{}
}

func newDecodeLedgerPublisher(store *Store, provider DecodeLedgerProvider) *decodeLedgerPublisher {
	return &decodeLedgerPublisher{
		store:     store,
		provider:  provider,
		dirty:     make(map[string]struct{}),
		published: make(map[string]struct{}),
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

func (p *decodeLedgerPublisher) markDirty(podKey string) {
	select {
	case <-p.done:
		return
	default:
	}
	p.mu.Lock()
	p.dirty[podKey] = struct{}{}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// close stops the publisher and, if its goroutine is running, waits for it to
// return, so a flush in progress cannot land after a replacement's writes.
func (p *decodeLedgerPublisher) close() {
	p.closeOnce.Do(func() { close(p.done) })
	if p.stopped != nil {
		<-p.stopped
	}
}

// start runs the publisher's goroutine.
func (p *decodeLedgerPublisher) start() {
	p.stopped = make(chan struct{})
	go func() {
		defer close(p.stopped)
		p.run()
	}()
}

func (p *decodeLedgerPublisher) run() {
	ticker := time.NewTicker(decodeLedgerRepublishInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.wake:
		case <-ticker.C:
			p.mu.Lock()
			for podKey := range p.published {
				p.dirty[podKey] = struct{}{}
			}
			p.mu.Unlock()
		case <-p.done:
			return
		}
		p.flush()
	}
}

// flush writes the current ledger of every dirty pod in one pipeline. A pod
// whose write fails stays dirty for the next flush.
func (p *decodeLedgerPublisher) flush() {
	p.mu.Lock()
	if len(p.dirty) == 0 {
		p.mu.Unlock()
		return
	}
	batch := make([]string, 0, len(p.dirty))
	for podKey := range p.dirty {
		batch = append(batch, podKey)
	}
	p.dirty = make(map[string]struct{})
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), runningRequestsWriteTimeout)
	defer cancel()
	// Charge times are published on the Redis clock, so the sums from different
	// gateways can be compared against one "now".
	offsetSeconds := float64(p.store.runningRequestsClockOffsetMillis.Load()) / 1000
	field := runningRequestsGatewayInstanceID
	pipe := p.store.redisClient.Pipeline()
	zero := make(map[string]bool, len(batch))
	for _, podKey := range batch {
		state := p.provider(podKey)
		key := decodeLedgerKey(podKey)
		if state.isZero() {
			zero[podKey] = true
			pipe.HDel(ctx, key, field)
			continue
		}
		sumAt := state.SumChargedAt + float64(state.Charges)*offsetSeconds
		pipe.HSet(ctx, key, field, formatLedgerValue(state.Tokens, state.Charges, sumAt))
		pipe.PExpire(ctx, key, runningRequestsTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		klog.V(4).ErrorS(err, "failed to publish decode ledger", "pod_count", len(batch))
		p.mu.Lock()
		for _, podKey := range batch {
			p.dirty[podKey] = struct{}{}
		}
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	for _, podKey := range batch {
		if zero[podKey] {
			delete(p.published, podKey)
		} else {
			p.published[podKey] = struct{}{}
		}
	}
	p.mu.Unlock()
}

func formatLedgerFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// GetPodsRunningRequestsAndDecodeLedger is GetPodsRunningRequests plus the
// decode ledger that the other live gateway replicas hold on each pod, read in
// the same Redis round trip. This gateway's own ledger is not included: the
// caller has it in memory. The ledger map is nil when Redis is not configured
// or the read failed, and has no entry for a pod no other live gateway has
// charged.
func (c *Store) GetPodsRunningRequestsAndDecodeLedger(pods []*v1.Pod) (map[string]int64, map[string]RemoteDecodeLoad, error) {
	live, ledger := c.readPodsSharedRoutingState(pods, true)
	return c.runningRequestsWithLocalFallback(pods, live), ledger, nil
}

// sumRemoteDecodeLedger adds up the fields of the live gateways other than this
// one, and enqueues the dead gateways' fields for pruning (never this
// gateway's own: it may not be in the live set yet, and it rewrites its field
// on its next change). ok is false when no other live gateway holds a charge
// on the pod.
func (c *Store) sumRemoteDecodeLedger(key string, fields map[string]string, live map[string]struct{}, nowSeconds float64) (RemoteDecodeLoad, bool) {
	var out RemoteDecodeLoad
	var n int64
	var sumChargedAt float64
	var dead []string
	found := false
	for gw, v := range fields {
		if gw == runningRequestsGatewayInstanceID {
			continue
		}
		if _, ok := live[gw]; !ok {
			dead = append(dead, gw)
			continue
		}
		tokens, charges, sumAt, ok := parseLedgerValue(v)
		if !ok || charges <= 0 {
			continue
		}
		// A charge can cost 0 tokens and still be decoding: count the gateway
		// whenever it holds a charge.
		found = true
		out.Tokens += math.Max(0, tokens)
		n += charges
		sumChargedAt += sumAt
	}
	c.enqueueDeadRunningRequestsPrune(key, dead)
	if !found {
		return RemoteDecodeLoad{}, false
	}
	if elapsed := float64(n)*nowSeconds - sumChargedAt; elapsed > 0 {
		out.Elapsed = elapsed
	}
	return out, true
}

// readPodsSharedRoutingState reads the live running-request counters for pods
// and, with withLedger, the other gateways' decode ledgers, in one pipeline.
// Both maps are nil when Redis is not configured or the read failed.
func (c *Store) readPodsSharedRoutingState(pods []*v1.Pod, withLedger bool) (map[string]int64, map[string]RemoteDecodeLoad) {
	if c.redisClient == nil || len(pods) == 0 {
		return nil, nil
	}
	valid := make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		if pod != nil {
			valid = append(valid, pod)
		}
	}
	if len(valid) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), runningRequestsReadTimeout)
	defer cancel()
	pipe := c.redisClient.Pipeline()
	liveCmd := pipe.ZRangeByScore(ctx, runningRequestsGatewaysKey, &redis.ZRangeBy{Min: c.liveGatewaysCutoffMillis(), Max: "+inf"})
	podKeys := make([]string, len(valid))
	redisKeys := make([]string, len(valid))
	hashCmds := make([]*redis.MapStringStringCmd, len(valid))
	var ledgerCmds []*redis.MapStringStringCmd
	if withLedger {
		ledgerCmds = make([]*redis.MapStringStringCmd, len(valid))
	}
	for i, pod := range valid {
		podKeys[i] = utils.GeneratePodKey(pod.Namespace, pod.Name)
		redisKeys[i] = runningRequestsKey(pod.Namespace, pod.Name)
		hashCmds[i] = pipe.HGetAll(ctx, redisKeys[i])
		if withLedger {
			ledgerCmds[i] = pipe.HGetAll(ctx, decodeLedgerKey(podKeys[i]))
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		klog.V(4).ErrorS(err, "failed to batch read running-requests counters", "pod_count", len(valid))
		return nil, nil
	}
	liveGateways, err := liveCmd.Result()
	if err != nil {
		klog.V(4).ErrorS(err, "failed to read live gateways for running-requests counters")
		return nil, nil
	}
	live := make(map[string]struct{}, len(liveGateways)+1)
	for _, gw := range liveGateways {
		live[gw] = struct{}{}
	}

	// See readPodRunningRequests: an empty live set does not make a hash's sum
	// untrustworthy -- a hash whose only-ever contributor(s) have gone stale
	// correctly sums to zero.
	counts := make(map[string]int64, len(valid))
	for i, cmd := range hashCmds {
		fields, err := cmd.Result()
		if err != nil || len(fields) == 0 {
			continue
		}
		// See overlaySelfRunningRequests: this gateway's own field must never be
		// trusted stale just because it already exists in the hash.
		c.overlaySelfRunningRequests(valid[i].Namespace, valid[i].Name, fields, live)
		total, excluded := sumLiveFields(fields, live)
		if len(excluded) > 0 {
			c.enqueueDeadRunningRequestsPrune(redisKeys[i], excluded)
		}
		counts[podKeys[i]] = total
	}
	if !withLedger {
		return counts, nil
	}

	nowSeconds := float64(c.redisNowMillis()) / 1000
	ledger := make(map[string]RemoteDecodeLoad)
	for i, cmd := range ledgerCmds {
		fields, err := cmd.Result()
		if err != nil || len(fields) == 0 {
			continue
		}
		if remote, ok := c.sumRemoteDecodeLedger(decodeLedgerKey(podKeys[i]), fields, live, nowSeconds); ok {
			ledger[podKeys[i]] = remote
		}
	}
	return counts, ledger
}
