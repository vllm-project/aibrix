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
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// The remote running reader (#3442, PR (a)) reads the other gateways' running counts from the
// aibrix:rrq:* hashes the request path already keeps live, instead of from the gateway snapshot.
// It is off by default. While it is off nothing here runs: the snapshot sync is the only source
// of GetPodsRunningRequestsApprox's and the realtime_num_requests_running gauge's remote counts.
const (
	remoteRunningReaderEnabledEnv  = "AIBRIX_REMOTE_RUNNING_READER_ENABLED"
	remoteRunningReaderIntervalEnv = "AIBRIX_REMOTE_RUNNING_READER_INTERVAL"
	// defaultRemoteRunningReaderInterval is the middle of the 500 ms-2 s range that cost the
	// same in the candidate simulation of #3442.
	defaultRemoteRunningReaderInterval = time.Second
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

// initRemoteRunningReader starts the reader when AIBRIX_REMOTE_RUNNING_READER_ENABLED is true and
// Redis is configured. Otherwise it does nothing, and store.remoteRunningReader stays nil, which
// keeps every consumer on the gateway snapshot.
func initRemoteRunningReader(store *Store, stopCh <-chan struct{}) {
	if store.redisClient == nil || !utils.LoadEnvBool(remoteRunningReaderEnabledEnv, false) {
		return
	}
	interval := utils.LoadEnvDuration(remoteRunningReaderIntervalEnv, defaultRemoteRunningReaderInterval)
	r := newRemoteRunningReader(store, interval)
	store.remoteRunningReader.Store(r)
	klog.InfoS("Initializing remote running reader (experimental)", "interval", interval, "maxAge", r.maxAge)
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

// sweep reads the live gateway set, then every known pod's aibrix:rrq hash in pipelined batches
// (N/100 + 1 round trips), within one interval. Each batch is published as soon as it is read,
// with the batch's start as its read time. A sweep that runs out of time or fails stops there:
// the pods it did not reach keep their previous entry and read time, so they keep aging, and the
// next sweep starts with them.
//
// TODO(#3442): skip pods a request refreshed within the last half interval.
func (r *remoteRunningReader) sweep() {
	c := r.store
	keys := c.metaPods.Keys()
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

	ctx, cancel := context.WithTimeout(context.Background(), r.interval)
	defer cancel()
	client := c.readClient()
	read := 0
	defer func() {
		// Advance by at least one batch, so a batch that fails every time cannot hold the sweep
		// at the same place.
		advance := read
		if advance == 0 {
			advance = min(remoteRunningReaderBatchSize, len(keys))
		}
		r.resumeAt = keys[advance%len(keys)]
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

	for start := 0; start < len(keys); start += remoteRunningReaderBatchSize {
		chunk := keys[start:min(start+remoteRunningReaderBatchSize, len(keys))]
		readAt := c.snapshotNow()
		pipe := client.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, len(chunk))
		for i, podKey := range chunk {
			cmds[i] = pipe.HGetAll(ctx, runningRequestsKeyPrefix+":"+podKey)
		}
		// As in readGatewaySnapshots: a Redis error reply for one key does not make its
		// batch-mates unread, but a timeout or dropped connection says nothing about any of them.
		if _, err := pipe.Exec(ctx); err != nil && !isRedisReply(err) {
			klog.V(4).ErrorS(err, "remote running reader stopped before reading every pod", "read", read, "pods", len(keys))
			r.errLog.log("read", err, "remote running reader cannot read every pod; the unread pods keep aging", "read", read, "pods", len(keys))
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

// freshCount is podKey's remote count while it was read within maxAge before now, and 0 after
// that, so the pod falls back to this gateway's own count. A count exactly maxAge old still
// counts, as in remoteRunningSnapshot.freshCount.
func (r *remoteRunningReader) freshCount(podKey string, now time.Time) int64 {
	e, ok := r.entries.Load(podKey)
	if !ok || now.Sub(e.readAt) > r.maxAge {
		return 0
	}
	return e.count
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

// sumOtherLiveRunning sums the running counts in a pod's aibrix:rrq hash of the live gateways
// other than this one. This gateway's own field is left out: its local atomic is always fresher.
// As in remoteRunningByPod, a value that is not a positive integer is skipped, and the sum
// saturates instead of wrapping.
func sumOtherLiveRunning(fields map[string]string, live map[string]struct{}) int64 {
	var sum int64
	for gw, v := range fields {
		if gw == runningRequestsGatewayInstanceID {
			continue
		}
		if _, ok := live[gw]; !ok {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		if n > math.MaxInt64-sum {
			return math.MaxInt64
		}
		sum += n
	}
	return sum
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
		if _, ok := r.store.metaPods.Load(podKey); !ok {
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
			Help:      "Pods known to this gateway at the end of the last remote running reader sweep, by whether their remote count was read within the age limit (fresh), longer ago (stale, not used), or never (unread)",
		}, []string{"state"}), "gateway_remote_running_reader_pods")
		remoteRunningReaderOldestAge = registerGauge(prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: constants.AibrixSubsystemName,
			Name:      "gateway_remote_running_reader_oldest_read_age_seconds",
			Help:      "Age, at the end of the last remote running reader sweep, of the least recently read remote count among the pods known to this gateway",
		}), "gateway_remote_running_reader_oldest_read_age_seconds")
	})
	remoteRunningReaderPods.WithLabelValues(remoteRunningPodFresh).Set(float64(fresh))
	remoteRunningReaderPods.WithLabelValues(remoteRunningPodStale).Set(float64(stale))
	remoteRunningReaderPods.WithLabelValues(remoteRunningPodUnread).Set(float64(unread))
	remoteRunningReaderOldestAge.Set(oldest.Seconds())
}
