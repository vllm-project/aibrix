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
// gateway's contribution until the next successful tick.
//
// Consumers call AggregateGatewaySnapshotsForPod to read from the in-memory cache
// without touching Redis.
func initGatewaySnapshotSync(store *Store, stopCh <-chan struct{}) {
	if store.redisClient == nil {
		return
	}
	ticker := time.NewTicker(gatewaySnapshotSyncInterval)
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
					}

					// Phase 2 (scan): collect all gateway snapshot keys across all instances.
					var allKeys []string
					var cursor uint64
					for {
						keys, nextCursor, err := store.redisClient.Scan(ctx, cursor, "aibrix:pod:*", 500).Result()
						if err != nil {
							klog.V(4).ErrorS(err, "failed to scan gateway snapshot keys; keeping previous snapshot")
							// An errored scan cannot tell us which keys are actually gone, so there
							// is nothing safe to rebuild the cache from this tick -- leave
							// store.gatewaySnapshotCache exactly as the last successful tick left
							// it instead of replacing it with an empty or partial map.
							return
						}
						allKeys = append(allKeys, keys...)
						cursor = nextCursor
						if cursor == 0 {
							break
						}
					}

					// Phase 2 (read): pipeline HGetAll in batches (bounded pipeline size per round-trip).
					// Built first as raw Redis key -> fields so a read failure for one key can fall
					// back to prevRaw's last known value for exactly that key, then grouped below into
					// the pod-keyed shape consumers read. A key missing from allKeys (this tick's
					// authoritative live set) is dropped rather than carried over: the scan
					// succeeded, so its absence is real, not a read failure.
					newRaw := make(map[string]map[string]string, len(allKeys))
					for batchStart := 0; batchStart < len(allKeys); batchStart += gatewaySnapshotHGetAllBatchSize {
						batchEnd := batchStart + gatewaySnapshotHGetAllBatchSize
						if batchEnd > len(allKeys) {
							batchEnd = len(allKeys)
						}
						chunk := allKeys[batchStart:batchEnd]
						readPipe := store.redisClient.Pipeline()
						cmds := make([]*redis.MapStringStringCmd, len(chunk))
						for i, key := range chunk {
							cmds[i] = readPipe.HGetAll(ctx, key)
						}
						if _, err := readPipe.Exec(ctx); err != nil {
							klog.V(4).ErrorS(err, "failed to pipeline HGetAll for gateway snapshots; keeping previous values for this batch's keys",
								"batchStart", batchStart, "batchLen", len(chunk))
							for _, key := range chunk {
								if old, ok := prevRaw[key]; ok {
									newRaw[key] = old
								}
							}
							continue
						}
						for i, cmd := range cmds {
							fields, err := cmd.Result()
							if err != nil {
								if old, ok := prevRaw[chunk[i]]; ok {
									newRaw[chunk[i]] = old
								}
								continue
							}
							if len(fields) > 0 {
								newRaw[chunk[i]] = fields
							}
						}
					}
					prevRaw = newRaw

					// Keyed by pod key (namespace/name) → list of per-gateway snapshots so consumers
					// can look up all snapshots for a pod in O(1) without scanning the full cache.
					newCache := make(map[string][]map[string]string, len(newRaw))
					for _, fields := range newRaw {
						pKey := utils.GeneratePodKey(fields["namespace"], fields["pod_name"])
						newCache[pKey] = append(newCache[pKey], fields)
					}

					// Atomically replace the snapshot cache so readers always see a consistent map.
					store.gatewaySnapshotCache.Store(newCache)
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
