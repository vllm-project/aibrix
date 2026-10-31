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
//  3. Stores the resulting map[redisKey]fields atomically in store.gatewaySnapshotCache.
//
// A scan failure keeps the previous snapshot untouched, and a read failure for one
// batch of keys keeps only those keys' last known values -- neither replaces the
// cache with an empty or partial map, which would otherwise drop every other
// gateway's contribution until the next successful tick. Before the first snapshot
// has been published there are no last known values to keep, so a tick that cannot
// read every key publishes nothing and the cache stays nil.
//
// Consumers call AggregateGatewaySnapshotsForPod to read from the in-memory cache
// without touching Redis.
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
						appendGatewayPodSnapshotToPipeline(ctx, writePipe, pod)
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
					newRaw, unread, readErr := readGatewaySnapshots(ctx, store.redisClient, allKeys, prevRaw)
					if readErr != nil {
						msg := "gateway snapshot sync cannot read some keys; using their previous values where there are any"
						if unread && prevRaw == nil {
							msg = "gateway snapshot sync cannot read some keys; not publishing the first snapshot yet"
						}
						errLog.log("read", readErr, msg, "keys", len(allKeys))
					}
					// Until one tick has read every key, there is no earlier snapshot to be wrong
					// relative to, and an empty or partial one would be taken as a full picture:
					// syncRunningRequestsGlobally skips only a nil cache. Leave the cache nil and
					// prevRaw unset, as a failed scan does, and try again next tick.
					if unread && prevRaw == nil {
						klog.V(4).InfoS("not publishing the first gateway snapshot: some keys could not be read", "keys", len(allKeys))
						return
					}
					prevRaw = newRaw
					newCache := groupSnapshotsByPod(newRaw)

					// Atomically replace the snapshot cache so readers always see a consistent map.
					store.gatewaySnapshotCache.Store(newCache)
					store.gatewayRemoteRunning.Store(&remoteRunningSnapshot{
						counts: remoteRunningByPod(newCache, self), readStart: start})
					klog.V(5).InfoS("refreshed gateway snapshot cache", "keys", len(newCache), "duration", time.Since(start))
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
	appendGatewayPodSnapshotToPipeline(ctx, pipe, pod)
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

// appendGatewayPodSnapshotToPipeline queues HSET + EXPIRE for a single pod onto an existing pipeline.
// The caller is responsible for calling pipe.Exec.
func appendGatewayPodSnapshotToPipeline(ctx context.Context, pipe redis.Pipeliner, pod *Pod) {
	key := gatewayPodSnapshotKey(gatewayPodName, pod.Namespace, pod.Name)
	fields := map[string]any{
		"gateway_instance_id": gatewayPodName,
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
// unread reports that some key failed to read and has no earlier value to stand in; readErr is
// the first failed read, for the rate-limited Error log.
func readGatewaySnapshots(ctx context.Context, client *redis.Client, allKeys []string, prevRaw map[string]map[string]string) (raw map[string]map[string]string, unread bool, readErr error) {
	raw = make(map[string]map[string]string, len(allKeys))
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
	return raw, unread, readErr
}

// groupSnapshotsByPod keys the per-gateway snapshots by pod key (namespace/name) so consumers can
// look up all snapshots for a pod in O(1) without scanning the full cache. A hash under the
// snapshot prefix that lacks either field was not written by a gateway (or is malformed); it has
// no pod to attach to, so grouping it would only add a bogus key such as "/".
func groupSnapshotsByPod(raw map[string]map[string]string) map[string][]map[string]string {
	grouped := make(map[string][]map[string]string, len(raw))
	for _, fields := range raw {
		namespace, podName := fields["namespace"], fields["pod_name"]
		if namespace == "" || podName == "" {
			continue
		}
		pKey := utils.GeneratePodKey(namespace, podName)
		grouped[pKey] = append(grouped[pKey], fields)
	}
	return grouped
}

// remoteRunningMaxAge is how old the other gateways' counts may be before
// GetPodsRunningRequestsApprox stops adding them. Ten sync intervals: a few missed ticks are
// tolerated, a stalled sync is not, so a gateway that cannot reach Redis falls back to its own
// counts instead of steering by a picture that keeps getting older.
const remoteRunningMaxAge = 10 * gatewaySnapshotSyncInterval

// remoteRunningSnapshot is the other gateways' running counts per pod key, and when the tick
// that read them started.
type remoteRunningSnapshot struct {
	counts    map[string]int64
	readStart time.Time
}

// remoteRunningByPod sums requests_running over the snapshots of every gateway instance other
// than self, per pod key. This gateway's own count is left out because its local atomic is
// always fresher than what it last wrote to Redis.
func remoteRunningByPod(grouped map[string][]map[string]string, self string) map[string]int64 {
	remote := make(map[string]int64, len(grouped))
	for podKey, snapshots := range grouped {
		var sum float64
		for _, fields := range snapshots {
			if fields["gateway_instance_id"] == self {
				continue
			}
			running, _ := strconv.ParseFloat(fields["requests_running"], 64)
			sum += running
		}
		if sum > 0 {
			remote[podKey] = int64(sum)
		}
	}
	return remote
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
