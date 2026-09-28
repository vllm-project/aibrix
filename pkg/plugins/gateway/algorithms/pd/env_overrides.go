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

package pd

import (
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

// EnvOverrides returns the PD routing knob defaults the environment configures:
// the decode abort timings, the decode load-balancing weights, the token-load
// ledger knobs, the hybrid prefix-cache thresholds and the bucket-serve
// switch and mode. The routing algorithm
// package folds them into the process default table at startup, and this
// package's tests install them the same way.
//
// AIBRIX_TOKEN_LOAD_MAX_SESSIONS has no counterpart here on purpose: it caps the
// shared session table of the token-load tracker, which every model of a
// gateway process shares, so it is not a per-request knob.
func EnvOverrides() types.PDOverrides {
	tokenLoad := DefaultTokenLoadConfig()
	hybrid := DefaultHybridCacheLoadConfig()
	bucketServe, bucketServeMode := EnvBucketServe()
	return types.PDOverrides{
		Abort: types.PDAbortOverrides{
			Timeout:    time.Duration(loadDecodeAbortTimeoutSeconds()) * time.Second,
			RetryDelay: time.Duration(loadDecodeAbortRetryDelayNanos()),
		},
		DecodeLB: types.PDDecodeLBOverrides{
			WeightRunning:    decodeLBWeightRunningReq,
			WeightThroughput: decodeLBWeightThroughput,
		},
		TokenLoad: types.PDTokenLoadOverrides{
			KVWeight:    tokenLoad.KVWeight,
			RequestCost: tokenLoad.RequestCost,
			TTL:         tokenLoad.TTL,
			SessionTTL:  tokenLoad.SessionTTL,
		},
		HybridCacheLoadFactor: hybrid.Factor,
		MinMatchPct:           hybrid.MinMatchPct,
		BucketServe:           bucketServe,
		BucketServeMode:       string(bucketServeMode),
	}
}

// EnvBucketServe returns the bucket-serve knobs the environment selects:
// AIBRIX_BUCKET_SERVE turns the adaptive plan on and
// AIBRIX_BUCKET_SERVE_MODE picks what its cut points balance. A config
// profile may then switch the plan, or select another mode, for the models it
// routes. An unknown mode name is refused with a warning and the default stays
// in place, the way the other environment loaders treat a value they would not
// accept.
func EnvBucketServe() (bool, BucketMode) {
	enabled := utils.LoadEnvBool("AIBRIX_BUCKET_SERVE", false)
	mode := BucketModeThroughput
	name := utils.LoadEnv("AIBRIX_BUCKET_SERVE_MODE", string(mode))
	if parsed, ok := ParseBucketMode(name); ok {
		mode = parsed
	} else {
		klog.Warningf("invalid AIBRIX_BUCKET_SERVE_MODE: %s, falling back to default: %s", name, mode)
	}
	return enabled, mode
}
