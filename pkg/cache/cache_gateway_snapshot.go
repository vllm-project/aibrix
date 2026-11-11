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
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	gatewaySnapshotSyncInterval = 100 * time.Millisecond
	gatewayPodSnapshotTTL       = 500 * time.Millisecond
	// gatewaySnapshotHGetAllBatchSize limits how many HGetAll commands share one pipeline Exec,
	// reducing head-of-line blocking and client memory spikes when many gateway snapshot keys exist.
	gatewaySnapshotHGetAllBatchSize = 100
	// gatewaySnapshotErrorLogInterval is how often a failing kind of snapshot tick (flush, scan,
	// read) is logged at Error level. The sync runs every 100ms, so an outage would otherwise
	// either flood the log or, at V(4), stay invisible while the snapshot silently goes stale.
	gatewaySnapshotErrorLogInterval = 30 * time.Second
)

var gatewayPodName = func() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	name, _ := os.Hostname()
	if name == "" {
		return "unknown-gateway"
	}
	return name
}()

// initGatewaySnapshotSync starts a background goroutine that every 100ms:
//  1. Writes this gateway's per-pod snapshots to Redis (one pipeline, one round-trip).
//  2. Scans all gateway snapshot keys (aibrix:pod:*) and pipelines HGetAll for every key.
//  3. Stores the resulting map[redisKey]fields atomically in store.gatewaySnapshotCache, and the
//     other gateways' counts per pod, with when each was read, in store.gatewayRemoteRunning.
//
// A scan failure keeps the previous snapshot untouched, and a read failure for one
// batch of keys keeps only those keys' last known values -- neither replaces the
// cache with an empty or partial map, which would otherwise drop every other
// gateway's contribution until the next successful tick. Before the first snapshot
// has been published there are no last known values to keep, so a tick that cannot
// read every key publishes nothing and the cache stays nil.
//
// Each published tick also stores gatewayRemoteRunning, the other gateways' counts per pod
// with when they were read. GetPodsRunningRequestsApprox and syncRunningRequestsGlobally read
// that without touching Redis.
func initGatewaySnapshotSync(store *Store, stopCh <-chan struct{}) {
	if store.redisClient == nil {
		return
	}
	ticker := time.NewTicker(gatewaySnapshotSyncInterval)
	errLog := store.gatewaySnapshotErrorLog
	if errLog == nil {
		errLog = newRateLimitedErrorLog(gatewaySnapshotErrorLogInterval)
	}
	// This gateway's identity does not change while it runs; read it once.
	self := gatewayPodName
	go func() {
		// prevRaw holds every gateway-instance key's last successfully-read fields,
		// keyed by its raw Redis key rather than by pod, so a read failure for one
		// key can fall back to exactly that key's last known value without
		// disturbing any other key aggregated under the same pod (see the read
		// phase below).
		var prevRaw map[string]map[string]string
		// prevReadAt holds, for the same keys, the start of the tick that last actually read
		// each one. A value carried over from prevRaw keeps its original read time, so the
		// candidate stage can tell a count read just now from one it has not been able to read
		// for many ticks (see remoteRunningSnapshot).
		var prevReadAt map[string]time.Time
		for {
			select {
			case <-ticker.C:
				// Bounded context per tick so a stuck Redis client cannot block this goroutine
				// indefinitely. Use a closure so defer cancel runs each iteration, not once at exit.
				func() {
					ctx, cancel := context.WithTimeout(context.Background(), gatewaySnapshotSyncInterval)
					defer cancel()
					start := time.Now()

					// Phase 1 (write): push all pods' snapshots in one pipeline.
					writePipe := store.redisClient.Pipeline()
					store.metaPods.Range(func(_ string, pod *Pod) bool {
						appendGatewayPodSnapshotToPipeline(ctx, writePipe, pod, self)
						return true
					})
					if _, err := writePipe.Exec(ctx); err != nil {
						klog.V(4).ErrorS(err, "failed to flush gateway pod snapshots")
						errLog.log("flush", err, "gateway snapshot sync cannot write this gateway's snapshots to Redis; other gateways will see them expire")
					}

					// Phase 2 (scan): collect all gateway snapshot keys across all instances.
					allKeys, err := scanGatewaySnapshotKeys(ctx, store.redisClient)
					if err != nil {
						klog.V(4).ErrorS(err, "failed to scan gateway snapshot keys; keeping previous snapshot")
						errLog.log("scan", err, "gateway snapshot sync cannot scan Redis; the snapshot is not being refreshed")
						// An errored scan cannot tell us which keys are actually gone, so there
						// is nothing safe to rebuild the cache from this tick -- leave
						// store.gatewaySnapshotCache exactly as the last successful tick left
						// it instead of replacing it with an empty or partial map.
						return
					}

					// Phase 2 (read): fetch every key's fields, falling back to prevRaw's last known value
					// for exactly the keys that could not be read (see readGatewaySnapshots).
					newRaw, carried, unread, readErr := readGatewaySnapshots(ctx, store.redisClient, allKeys, prevRaw)
					if readErr != nil {
						msg := "gateway snapshot sync cannot read some keys; carrying their last values over, which stop counting once older than remoteRunningMaxAge"
						if unread && prevRaw == nil {
							msg = "gateway snapshot sync cannot read some keys; not publishing the first snapshot yet"
						}
						errLog.log("read", readErr, msg, "keys", len(allKeys))
					}
					// Until one tick has read every key, there is no earlier snapshot to be wrong
					// relative to, and an empty or partial one would be taken as a full picture:
					// syncRunningRequestsGlobally skips only an unset gatewayRemoteRunning. Leave
					// both unset and prevRaw too, as a failed scan does, and try again next tick.
					if unread && prevRaw == nil {
						klog.V(4).InfoS("not publishing the first gateway snapshot: some keys could not be read", "keys", len(allKeys))
						return
					}
					newReadAt := make(map[string]time.Time, len(newRaw))
					for key := range newRaw {
						if _, ok := carried[key]; ok {
							newReadAt[key] = prevReadAt[key]
						} else {
							newReadAt[key] = start
						}
					}
					prevRaw, prevReadAt = newRaw, newReadAt

					// Atomically replace the snapshot cache so readers always see a consistent map.
					store.gatewaySnapshotCache.Store(newRaw)
					store.gatewayRemoteRunning.Store(remoteRunningByPod(newRaw, newReadAt, self))
					klog.V(5).InfoS("refreshed gateway snapshot cache", "keys", len(newRaw), "duration", time.Since(start))
				}()

			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

// UpsertGatewayPodSnapshot writes this gateway instance's snapshot for a single pod to Redis.
// The key is scoped to this gateway instance via POD_NAME.
func (c *Store) UpsertGatewayPodSnapshot(ctx context.Context, pod *Pod) error {
	if c.redisClient == nil {
		return nil
	}
	pipe := c.redisClient.Pipeline()
	appendGatewayPodSnapshotToPipeline(ctx, pipe, pod, gatewayPodName)
	_, err := pipe.Exec(ctx)
	return err
}

// GetGatewayPodSnapshot reads this gateway instance's snapshot fields for a pod from Redis.
func (c *Store) GetGatewayPodSnapshot(ctx context.Context, pod *Pod) (map[string]string, error) {
	if c.redisClient == nil {
		return nil, nil
	}
	key := gatewayPodSnapshotKey(gatewayPodName, pod.Namespace, pod.Name)
	result, err := c.redisClient.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get gateway pod snapshot for pod %s: %w",
			utils.GeneratePodKey(pod.Namespace, pod.Name), err)
	}
	return result, nil
}

// GetAllGatewayPodSnapshots reads snapshots from all gateway instances for a pod by scanning
// keys matching the pod's namespace and name across all gateway instances.
func (c *Store) GetAllGatewayPodSnapshots(ctx context.Context, pod *Pod) ([]map[string]string, error) {
	if c.redisClient == nil {
		return nil, nil
	}
	pattern := gatewayPodSnapshotPattern(pod.Namespace, pod.Name)

	var snapshots []map[string]string
	var cursor uint64
	for {
		keys, nextCursor, err := c.redisClient.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, fmt.Errorf("failed to scan gateway pod snapshots for pod %s: %w",
				utils.GeneratePodKey(pod.Namespace, pod.Name), err)
		}
		for _, key := range keys {
			fields, err := c.redisClient.HGetAll(ctx, key).Result()
			if err != nil {
				continue
			}
			snapshots = append(snapshots, fields)
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return snapshots, nil
}

// appendGatewayPodSnapshotToPipeline queues HSET + EXPIRE for a single pod onto an existing pipeline,
// as gateway instance gatewayName. The sync loop passes the name it read once at startup, so its
// goroutine never reads gatewayPodName again. The caller is responsible for calling pipe.Exec.
func appendGatewayPodSnapshotToPipeline(ctx context.Context, pipe redis.Pipeliner, pod *Pod, gatewayName string) {
	key := gatewayPodSnapshotKey(gatewayName, pod.Namespace, pod.Name)
	fields := map[string]any{
		"gateway_instance_id": gatewayName,
		"pod_uid":             string(pod.UID),
		"pod_name":            pod.Name,
		"namespace":           pod.Namespace,
		"node_name":           pod.Spec.NodeName,
		"requests_running":    strconv.Itoa(int(atomic.LoadInt32(&pod.runningRequests))),
		"seq":                 strconv.FormatInt(atomic.LoadInt64(&pod.completedRequests), 10),
		"update_time":         time.Now().Format("15:04:05.000"),
	}
	pipe.HSet(ctx, key, fields)
	pipe.PExpire(ctx, key, gatewayPodSnapshotTTL)
}

// gatewayPodSnapshotKey returns the Redis key for this gateway instance's snapshot of a pod.
// Format: aibrix:pod:{gatewayPodName}:{namespace}:{podName}
func gatewayPodSnapshotKey(gatewayName, namespace, podName string) string {
	return fmt.Sprintf("aibrix:pod:%s:%s:%s", gatewayName, namespace, podName)
}

// gatewayPodSnapshotPattern returns a SCAN pattern matching all gateway snapshots for a pod.
// Format: aibrix:pod:*:{namespace}:{podName}
func gatewayPodSnapshotPattern(namespace, podName string) string {
	return fmt.Sprintf("aibrix:pod:*:%s:%s", namespace, podName)
}

// scanGatewaySnapshotKeys returns every gateway snapshot key (aibrix:pod:*) across all instances.
func scanGatewaySnapshotKeys(ctx context.Context, client *redis.Client) ([]string, error) {
	var allKeys []string
	var cursor uint64
	for {
		keys, nextCursor, err := client.Scan(ctx, cursor, "aibrix:pod:*", 500).Result()
		if err != nil {
			return nil, err
		}
		allKeys = append(allKeys, keys...)
		cursor = nextCursor
		if cursor == 0 {
			return allKeys, nil
		}
	}
}

// readGatewaySnapshots pipelines HGetAll over allKeys in batches (bounded pipeline size per
// round-trip) and returns raw Redis key -> fields, so a read failure for one key can fall back
// to prevRaw's last known value for exactly that key without disturbing any other key under the
// same pod. A key missing from allKeys (the scan's authoritative live set) is dropped rather
// than carried over: the scan succeeded, so its absence is real, not a read failure.
//
// Each key is judged by its own command's result, not by the pipeline's error: Exec reports
// only the first failure, so one key Redis rejects (see below) must not make its 99 batch-mates
// look failed. The exception is a failure that is not a Redis reply -- a timeout, a dropped
// connection -- which says nothing reliable about any command in the batch, so the whole batch
// counts as unread.
//
// carried holds the keys whose value in raw is prevRaw's, not one read this tick. unread reports
// that some key failed to read and has no earlier value to stand in; readErr is the first failed
// read, for the rate-limited Error log.
func readGatewaySnapshots(ctx context.Context, client *redis.Client, allKeys []string, prevRaw map[string]map[string]string) (raw map[string]map[string]string, carried map[string]struct{}, unread bool, readErr error) {
	raw = make(map[string]map[string]string, len(allKeys))
	carried = make(map[string]struct{})
	for batchStart := 0; batchStart < len(allKeys); batchStart += gatewaySnapshotHGetAllBatchSize {
		batchEnd := batchStart + gatewaySnapshotHGetAllBatchSize
		if batchEnd > len(allKeys) {
			batchEnd = len(allKeys)
		}
		chunk := allKeys[batchStart:batchEnd]
		readPipe := client.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, len(chunk))
		for i, key := range chunk {
			cmds[i] = readPipe.HGetAll(ctx, key)
		}
		_, execErr := readPipe.Exec(ctx)
		batchFailed := execErr != nil && !isRedisReply(execErr)
		if batchFailed {
			klog.V(4).ErrorS(execErr, "failed to pipeline HGetAll for gateway snapshots; keeping previous values for this batch's keys",
				"batchStart", batchStart, "batchLen", len(chunk))
		}
		for i, cmd := range cmds {
			fields, err := cmd.Result()
			switch {
			case batchFailed || (err != nil && !isRedisReply(err)):
				if readErr == nil {
					readErr = err
					if batchFailed {
						readErr = execErr
					}
				}
				if old, ok := prevRaw[chunk[i]]; ok {
					raw[chunk[i]] = old
					carried[chunk[i]] = struct{}{}
				} else {
					unread = true
				}
			case err != nil:
				// Redis answered with an error for this key alone (WRONGTYPE for a key
				// something else wrote under the prefix). It is not a gateway snapshot and
				// will fail the same way every tick, so it is skipped; treating it as
				// unread would freeze the snapshot behind it.
				klog.V(5).ErrorS(err, "skipping non-snapshot key under the gateway snapshot prefix", "key", chunk[i])
			case len(fields) > 0:
				raw[chunk[i]] = fields
			}
		}
	}
	return raw, carried, unread, readErr
}

// remoteRunningMaxAge is how old the other gateways' counts for a pod may be before
// GetPodsRunningRequestsApprox stops adding them. Ten sync intervals: a few missed ticks are
// tolerated, a stalled sync is not, so a gateway that cannot reach Redis falls back to its own
// counts instead of steering by a picture that keeps getting older. The age is judged per pod,
// from when its counts were actually read: a tick that cannot read some keys still publishes,
// carrying their last known values over, and a tick that keeps running out of time misses the
// same keys every time (#3441).
const remoteRunningMaxAge = 10 * gatewaySnapshotSyncInterval

// remoteRunningSnapshot is the other gateways' running counts per pod key: one entry per other
// gateway's snapshot of the pod (at most G-1), each with the start of the tick that actually read
// it. Snapshots reporting no running requests are left out.
type remoteRunningSnapshot struct {
	entries map[string][]remoteRunningEntry
}

type remoteRunningEntry struct {
	count  int64
	readAt time.Time
}

// freshCount is the other gateways' running count for podKey: the sum over its snapshots that
// were actually read within remoteRunningMaxAge before now. Each snapshot is judged on its own,
// because a tick that runs out of time leaves a scattered share of keys unread: judging a pod by
// its oldest snapshot would drop the remote count of most pods once there are several gateways,
// while summing only the fresh ones keeps what was read. A pod whose snapshots are all stale
// (a stalled sync) gets 0, so it falls back to this gateway's own count. The candidate stage's
// estimate and the realtime_num_requests_running gauge both go through it, so the two never
// disagree about which counts are too old to use.
func (s *remoteRunningSnapshot) freshCount(podKey string, now time.Time) int64 {
	var sum int64
	for _, e := range s.entries[podKey] {
		if now.Sub(e.readAt) <= remoteRunningMaxAge {
			sum += e.count
		}
	}
	return sum
}

// remoteRunningByPod collects requests_running from the snapshots of every gateway instance other
// than self, per pod key, from raw (Redis key -> fields) and readAt (Redis key -> start of the
// tick that read it), keeping each snapshot's read time so freshCount can judge it on its own.
// This gateway's own count is left out because its local atomic is always fresher than what it
// last wrote to Redis. A snapshot without pod identity was not written by a gateway (or is
// malformed) and has no pod to attach to, so it is skipped.
//
// These per-pod remote counts have a second consumer besides the realtime_num_requests_running
// gauge: the routing candidate stage reads them through GetPodsRunningRequestsApprox to rank
// candidates. A change that replaces or slows this sync (#3322) must keep publishing them, or
// the stage quietly ranks on this gateway's own counts alone.
func remoteRunningByPod(raw map[string]map[string]string, readAt map[string]time.Time, self string) *remoteRunningSnapshot {
	snap := &remoteRunningSnapshot{entries: make(map[string][]remoteRunningEntry)}
	for key, fields := range raw {
		namespace, podName := fields["namespace"], fields["pod_name"]
		if namespace == "" || podName == "" || fields["gateway_instance_id"] == self {
			continue
		}
		running, _ := strconv.ParseFloat(fields["requests_running"], 64)
		if running <= 0 {
			continue
		}
		podKey := utils.GeneratePodKey(namespace, podName)
		snap.entries[podKey] = append(snap.entries[podKey], remoteRunningEntry{count: int64(running), readAt: readAt[key]})
	}
	return snap
}

// isRedisReply reports whether err is an error reply from the Redis server (such as WRONGTYPE),
// as opposed to a failure to get a reply at all (a timeout, a dropped connection).
func isRedisReply(err error) bool {
	var re redis.Error
	return errors.As(err, &re)
}

// rateLimitedErrorLog emits an Error-level line for a recurring failure at most once per
// interval for each named event, and says how many occurrences the line stands for. It is for
// a loop that retries every 100ms: logging each failure would flood the log, and logging none
// at Error level would leave a snapshot that has silently stopped refreshing invisible.
// Not safe for concurrent use; the snapshot sync goroutine is its only caller.
type rateLimitedErrorLog struct {
	interval time.Duration
	now      func() time.Time
	emit     func(err error, msg string, keysAndValues ...interface{})
	events   map[string]*rateLimitedErrorEvent
}

type rateLimitedErrorEvent struct {
	last       time.Time
	suppressed int
}

func newRateLimitedErrorLog(interval time.Duration) *rateLimitedErrorLog {
	return &rateLimitedErrorLog{
		interval: interval,
		now:      time.Now,
		emit:     klog.ErrorS,
		events:   map[string]*rateLimitedErrorEvent{},
	}
}

// log emits msg for event unless one was emitted for it less than interval ago, in which case
// the occurrence is only counted toward the next line's "suppressed" value.
func (l *rateLimitedErrorLog) log(event string, err error, msg string, keysAndValues ...interface{}) {
	ev := l.events[event]
	if ev == nil {
		ev = &rateLimitedErrorEvent{}
		l.events[event] = ev
	}
	now := l.now()
	if !ev.last.IsZero() && now.Sub(ev.last) < l.interval {
		ev.suppressed++
		return
	}
	keysAndValues = append(keysAndValues, "suppressedSinceLastLog", ev.suppressed)
	ev.last, ev.suppressed = now, 0
	l.emit(err, msg, keysAndValues...)
}
