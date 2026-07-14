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

package modelclaim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

const poolReclaimModeKVFirst = "kv-first"

var errPoolKVUsageExceedsCapacity = errors.New(
	"observed KV usage and protected floors exceed pool capacity",
)

// poolPolicy is intentionally configured by one JSON Deployment annotation,
// not another resource. Reclaim is KV-first by construction: it adjusts
// kvcached ceilings before any future weight eviction policy is considered.
type poolPolicy struct {
	Reclaim   *poolReclaimPolicy   `json:"reclaim,omitempty"`
	Lifecycle *poolLifecyclePolicy `json:"lifecycle,omitempty"`
}

type poolReclaimPolicy struct {
	Mode                   string `json:"mode,omitempty"`
	CapacityBytes          int64  `json:"capacityBytes"`
	GuaranteedFloorPercent int32  `json:"guaranteedFloorPercent,omitempty"`
}

type poolLifecyclePolicy struct {
	SleepAfterSeconds int64 `json:"sleepAfterSeconds,omitempty"`
}

// parsePoolPolicy decodes the Deployment policy annotation and rejects unknown
// keys. A typo must disable policy safely rather than silently changing GPU
// memory behavior.
func parsePoolPolicy(raw string) (*poolPolicy, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	policy := &poolPolicy{}
	if err := decoder.Decode(policy); err != nil {
		return nil, fmt.Errorf("decode pool policy: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode pool policy: multiple JSON values")
		}
		return nil, fmt.Errorf("decode pool policy: %w", err)
	}
	if policy.Reclaim != nil {
		if policy.Reclaim.Mode == "" {
			policy.Reclaim.Mode = poolReclaimModeKVFirst
		}
		if policy.Reclaim.Mode != poolReclaimModeKVFirst {
			return nil, fmt.Errorf("reclaim.mode must be %q", poolReclaimModeKVFirst)
		}
		if policy.Reclaim.CapacityBytes <= 0 {
			return nil, fmt.Errorf("reclaim.capacityBytes must be positive")
		}
		if policy.Reclaim.GuaranteedFloorPercent < 0 ||
			policy.Reclaim.GuaranteedFloorPercent > 100 {
			return nil, fmt.Errorf("reclaim.guaranteedFloorPercent must be between 0 and 100")
		}
	}
	if policy.Lifecycle != nil && policy.Lifecycle.SleepAfterSeconds < 0 {
		return nil, fmt.Errorf("lifecycle.sleepAfterSeconds must not be negative")
	}
	return policy, nil
}

type poolRequestActivity struct {
	Active           bool
	RequestsInFlight int64
	CompletionDelta  int64
}

type poolKVModel struct {
	Name            string
	KVUsedBytes     int64
	KVCapacityBytes int64
	Activity        poolRequestActivity
}

// computePoolKVTargets distributes one explicit physical KV budget across the
// engines on a single GPU. Current KV usage is a hard lower bound: if retained
// pages plus floors already exceed the budget, it returns no plan instead of
// asking kvctl to force an unsafe shrink.
func computePoolKVTargets(
	capacityBytes int64,
	guaranteedFloorPercent int32,
	models []poolKVModel,
) (map[string]int64, error) {
	if capacityBytes <= 0 {
		return nil, fmt.Errorf("pool capacity must be positive")
	}
	if guaranteedFloorPercent < 0 || guaranteedFloorPercent > 100 {
		return nil, fmt.Errorf("guaranteed floor percent must be between 0 and 100")
	}
	if len(models) == 0 {
		return nil, nil
	}

	floorBytes := percentOf(capacityBytes, guaranteedFloorPercent)
	targets := make(map[string]int64, len(models))
	baseBytes := int64(0)
	hasActiveModel := false
	active := make([]poolKVModel, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model.Name == "" {
			return nil, fmt.Errorf("pool model name must not be empty")
		}
		if _, found := seen[model.Name]; found {
			return nil, fmt.Errorf("duplicate pool model %q", model.Name)
		}
		seen[model.Name] = struct{}{}
		used := max(model.KVUsedBytes, 0)
		base := max(floorBytes, used)
		if baseBytes > capacityBytes-base {
			return nil, errPoolKVUsageExceedsCapacity
		}
		baseBytes += base
		targets[model.Name] = base
		if model.Activity.Active {
			hasActiveModel = true
			active = append(active, model)
		}
	}
	// Without demonstrable demand there is no pressure signal. Preserve the
	// existing limits rather than shrinking idle engines merely because a
	// controller just started or saw a quiet interval.
	if !hasActiveModel {
		return nil, nil
	}

	remaining := capacityBytes - baseBytes
	if remaining <= 0 {
		return targets, nil
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Name < active[j].Name })
	weights := make(map[string]int64, len(active))
	totalWeight := int64(0)
	for _, model := range active {
		weight := int64(1) + boundedActivity(model.Activity.RequestsInFlight) +
			boundedActivity(model.Activity.CompletionDelta)
		weights[model.Name] = weight
		totalWeight += weight
	}
	if totalWeight == 0 {
		return targets, nil
	}

	granted := int64(0)
	for _, model := range active {
		grant := remaining * weights[model.Name] / totalWeight
		targets[model.Name] += grant
		granted += grant
	}
	// Give deterministic ownership to rounding remainders so successive ticks
	// converge instead of oscillating on a one-byte difference.
	for index := int64(0); index < remaining-granted; index++ {
		name := active[index%int64(len(active))].Name
		targets[name]++
	}
	return targets, nil
}

func boundedActivity(value int64) int64 {
	if value < 0 {
		return 0
	}
	if value > 4 {
		return 4
	}
	return value
}

func percentOf(value int64, percent int32) int64 {
	return value/100*int64(percent) + value%100*int64(percent)/100
}
