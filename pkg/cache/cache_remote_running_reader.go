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
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// The remote running reader (#3442) reads the other gateways' running counts from the
// aibrix:rrq:* hashes the request path already keeps live, instead of from the gateway snapshot.
// It is on by default; set AIBRIX_REMOTE_RUNNING_READER_ENABLED=false to fall back to the
// gateway snapshot for both GetPodsRunningRequestsApprox and the realtime_num_requests_running
// gauge.
const (
	remoteRunningReaderEnabledEnv  = "AIBRIX_REMOTE_RUNNING_READER_ENABLED"
	remoteRunningReaderIntervalEnv = "AIBRIX_REMOTE_RUNNING_READER_INTERVAL"
	// defaultRemoteRunningReaderInterval is the middle of the 500 ms-2 s range that cost the
	// same in the candidate simulation of #3442.
	defaultRemoteRunningReaderInterval = time.Second
	// minRemoteRunningReaderInterval clamps mistyped small durations (e.g., "1ms" for "1s")
	// so sweeps cannot degenerate into a tight continuous HGETALL loop.
	minRemoteRunningReaderInterval = 100 * time.Millisecond
	// remoteRunningReaderMaxAgeIntervals: a pod's remote count is used while it was read within
	// this many intervals, so two consecutive failed sweeps (a GC pause, a Redis blip) are
	// tolerated before the pod falls back to this gateway's own count.
	remoteRunningReaderMaxAgeIntervals = 3
	// remoteRunningReaderBatchSize limits how many HGETALLs share one pipeline, as
	// gatewaySnapshotHGetAllBatchSize does for the snapshot.
	remoteRunningReaderBatchSize = 100
)

// remoteRunningReader holds, per pod key, the other live gateways' summed running count with the
// start of the read that produced it. Each entry is immutable and replaced whole, and a newer
// read always wins (see publish). Two writers keep it fresh:
//   - the sweep, about once per interval, over this gateway's known pods;
//   - every successful exact read on the request path (readPodsSharedRoutingState), with the
//     fields it already has, at no extra Redis command.
type remoteRunningReader struct {
	store    *Store
	interval time.Duration
	maxAge   time.Duration
	entries  utils.SyncMap[string, *remoteRunningEntry]

	// resumeAt is the pod key the next sweep starts at, so a sweep that runs out of time is
	// followed by one that starts with the pods it did not reach. Only the sweep goroutine
	// touches it, and errLog.
	resumeAt string
	errLog   *rateLimitedErrorLog
}

func newRemoteRunningReader(store *Store, interval time.Duration) *remoteRunningReader {
	return &remoteRunningReader{
		store:    store,
		interval: interval,
		maxAge:   remoteRunningReaderMaxAgeIntervals * interval,
		errLog:   newRateLimitedErrorLog(gatewaySnapshotErrorLogInterval),
	}
}

// initRemoteRunningReader starts the reader unless AIBRIX_REMOTE_RUNNING_READER_ENABLED is false
// and whenever Redis is configured. When disabled or without Redis, store.remoteRunningReader
// stays nil and consumers stay on the gateway snapshot.
func initRemoteRunningReader(store *Store, stopCh <-chan struct{}) {
	if store.redisClient == nil || !utils.LoadEnvBool(remoteRunningReaderEnabledEnv, true) {
		return
	}
	interval := utils.LoadEnvDuration(remoteRunningReaderIntervalEnv, defaultRemoteRunningReaderInterval)
	if interval < minRemoteRunningReaderInterval {
		klog.Warningf("%s=%s is below minimum %s; clamping to %s",
			remoteRunningReaderIntervalEnv, interval, minRemoteRunningReaderInterval, minRemoteRunningReaderInterval)
		interval = minRemoteRunningReaderInterval
	}
	r := newRemoteRunningReader(store, interval)
	store.remoteRunningReader.Store(r)
	klog.InfoS("Initializing remote running reader", "interval", interval, "maxAge", r.maxAge)
	go r.run(stopCh)
}

func (r *remoteRunningReader) run(stopCh <-chan struct{}) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		r.sweep()
		select {
		case <-ticker.C:
		case <-stopCh:
			return
		}
	}
}

// sweep reads the live gateway set, then every known ready pod's aibrix:rrq hash in pipelined
// batches (N/100 + 1 round trips), within one interval, skipping pods whose entry was already
// refreshed within the last half interval (for instance, by an exact read on the request path).
// Each batch is published as soon as it is read, with the batch's start as its read time. A sweep
// that runs out of time or fails stops there: the pods it did not reach keep their previous entry
// and read time, so they keep aging, and the next sweep starts with them.
func (r *remoteRunningReader) sweep() {
	c := r.store
	rawKeys := c.metaPods.Keys()
	keys := make([]string, 0, len(rawKeys))
	for _, podKey := range rawKeys {
		metaPod, ok := c.metaPods.Load(podKey)
		if !ok || metaPod.Pod == nil || !utils.FilterReadyPod(metaPod.Pod) {
			continue
		}
		keys = append(keys, podKey)
	}
	if len(keys) == 0 {
		r.pruneDeletedPods()
		r.recordFreshness(keys)
		return
	}
	// metaPods has no stable order, so sort to make "start where the last sweep stopped" mean
	// something, then rotate.
	sort.Strings(keys)
	first := sort.SearchStrings(keys, r.resumeAt) % len(keys)
	keys = append(keys[first:len(keys):len(keys)], keys[:first]...)

	sweepStart := c.snapshotNow()
	halfInterval := r.interval / 2
	toRead := make([]string, 0, len(keys))
	for _, podKey := range keys {
		if e, ok := r.entries.Load(podKey); ok && sweepStart.Sub(e.readAt) < halfInterval {
			continue
		}
		toRead = append(toRead, podKey)
	}
	if len(toRead) == 0 {
		r.pruneDeletedPods()
		r.recordFreshness(keys)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.interval)
	defer cancel()
	client := c.readClient()
	read := 0
	defer func() {
		// Advance by at least one batch, so a batch that fails every time cannot hold the sweep
		// at the same place.
		advance := read
		if advance == 0 {
			advance = min(remoteRunningReaderBatchSize, len(toRead))
		}
		r.resumeAt = toRead[advance%len(toRead)]
		r.pruneDeletedPods()
		r.recordFreshness(keys)
	}()

	liveGateways, err := client.ZRangeByScore(ctx, runningRequestsGatewaysKey,
		&redis.ZRangeBy{Min: c.liveGatewaysCutoffMillis(), Max: "+inf"}).Result()
	if err != nil {
		klog.V(4).ErrorS(err, "remote running reader cannot read the live gateways")
		r.errLog.log("live", err, "remote running reader cannot read the live gateways; remote counts are not being refreshed")
		return
	}
	live := make(map[string]struct{}, len(liveGateways))
	for _, gw := range liveGateways {
		live[gw] = struct{}{}
	}

	for start := 0; start < len(toRead); start += remoteRunningReaderBatchSize {
		chunk := toRead[start:min(start+remoteRunningReaderBatchSize, len(toRead))]
		readAt := c.snapshotNow()
		pipe := client.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, len(chunk))
		for i, podKey := range chunk {
			cmds[i] = pipe.HGetAll(ctx, runningRequestsKeyForPodKey(podKey))
		}
		// As in readGatewaySnapshots: a Redis error reply for one key does not make its
		// batch-mates unread, but a timeout or dropped connection says nothing about any of them.
		if _, err := pipe.Exec(ctx); err != nil && !isRedisReply(err) {
			klog.V(4).ErrorS(err, "remote running reader stopped before reading every pod", "read", read, "pods", len(toRead))
			r.errLog.log("read", err, "remote running reader cannot read every pod; the unread pods keep aging", "read", read, "pods", len(toRead))
			return
		}
		for i, cmd := range cmds {
			fields, err := cmd.Result()
			if err != nil {
				continue // not an rrq hash (WRONGTYPE): left unread
			}
			r.publish(chunk[i], sumOtherLiveRunning(fields, live), readAt)
		}
		read += len(chunk)
	}
}

// publish stores podKey's remote count read at readAt, unless an entry from the same or a later
// read time is already there: a slow sweep batch must not overwrite a fresher request-path read,
// nor the other way round. Equal read times keep the entry already published.
func (r *remoteRunningReader) publish(podKey string, count int64, readAt time.Time) {
	if old, loaded := r.entries.Load(podKey); loaded && !old.readAt.Before(readAt) {
		return
	}
	e := &remoteRunningEntry{count: count, readAt: readAt}
	for {
		old, loaded := r.entries.LoadOrStore(podKey, e)
		if !loaded || !old.readAt.Before(readAt) {
			return
		}
		if r.entries.CompareAndSwap(podKey, old, e) {
			return
		}
	}
}

// freshCount returns podKey's remote count and whether an entry has been read for podKey yet.
// Once read, the remote count is returned while within maxAge before now (exactly maxAge old
// still counts, as in remoteRunningSnapshot.freshCount), and 0 after that so the pod falls back
// to this gateway's own count. Callers emitting gauges (syncRunningRequestsGlobally) skip
// unread pods, matching #3325.
func (r *remoteRunningReader) freshCount(podKey string, now time.Time) (int64, bool) {
	e, ok := r.entries.Load(podKey)
	if !ok {
		return 0, false
	}
	if now.Sub(e.readAt) > r.maxAge {
		return 0, true
	}
	return e.count, true
}

// pruneDeletedPods drops the entries of pods this gateway no longer knows.
func (r *remoteRunningReader) pruneDeletedPods() {
	r.entries.Range(func(podKey string, _ *remoteRunningEntry) bool {
		if _, ok := r.store.metaPods.Load(podKey); !ok {
			r.entries.Delete(podKey)
		}
		return true
	})
}

// States of a known pod's remote count at the end of a sweep, the state label of
// aibrix_gateway_remote_running_reader_pods.
const (
	remoteRunningPodFresh  = "fresh"  // read within maxAge
	remoteRunningPodStale  = "stale"  // read, but longer ago than maxAge: not used
	remoteRunningPodUnread = "unread" // never read
)

var (
	remoteRunningReaderMetricsOnce sync.Once
	remoteRunningReaderPods        *prometheus.GaugeVec
	remoteRunningReaderOldestAge   prometheus.Gauge
)

// recordFreshness sets the reader's freshness gauges over podKeys at the end of a sweep. They are
// registered only once the reader runs, so a gateway with the reader off exports nothing new.
func (r *remoteRunningReader) recordFreshness(podKeys []string) {
	now := r.store.snapshotNow()
	var fresh, stale, unread int
	var oldest time.Duration
	for _, podKey := range podKeys {
		mp, ok := r.store.metaPods.Load(podKey)
		if !ok || mp.Pod == nil || !utils.FilterReadyPod(mp.Pod) {
			continue
		}
		e, ok := r.entries.Load(podKey)
		if !ok {
			unread++
			continue
		}
		age := now.Sub(e.readAt)
		if age > r.maxAge {
			stale++
		} else {
			fresh++
		}
		oldest = max(oldest, age)
	}
	remoteRunningReaderMetricsOnce.Do(func() {
		remoteRunningReaderPods = registerGauge(prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_remote_running_reader_pods",
			Help:      "Pods selected as ready (ready, with IP, non-terminating, non-draining) at sweep start that remain ready at sweep end, by whether their remote count was read within the age limit (fresh), longer ago (stale, not used), or never (unread)",
		}, []string{"state"}), "gateway_remote_running_reader_pods")
		remoteRunningReaderOldestAge = registerGauge(prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_remote_running_reader_oldest_read_age_seconds",
			Help:      "Age, at the end of the last remote running reader sweep, of the least recently read remote count among the sweep's ready pods that remained ready at sweep end",
		}), "gateway_remote_running_reader_oldest_read_age_seconds")
	})
	remoteRunningReaderPods.WithLabelValues(remoteRunningPodFresh).Set(float64(fresh))
	remoteRunningReaderPods.WithLabelValues(remoteRunningPodStale).Set(float64(stale))
	remoteRunningReaderPods.WithLabelValues(remoteRunningPodUnread).Set(float64(unread))
	remoteRunningReaderOldestAge.Set(oldest.Seconds())
}
