/*
Copyright 2024 The Aibrix Team.

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

package prefixcacheindexer

import (
	"time"

	v1 "k8s.io/api/core/v1"
)

type PrefixCacheIndexer interface {
	// MatchPrefix matches the longest prefix sequence for input request (passed as input tokens)
	// and returns matched prefix (as tokens), remaining unmatched input request (as tokens) and pods matching the prefix
	MatchPrefix(inputTokens []string, model string, pods []*v1.Pod) (matchedTokens []string, unMatchedTokens []string, matchedPods []*v1.Pod)

	// AddPrefix adds tokens in internal prefix cache indexer to be used by future requests
	AddPrefix(tokens []string, model, pod string)

	// Evict is invoked at fixed internal to clean up expired tokens from prefix cache.
	// TODO: Add max blocks to cache, add LRU policy along with TTL and add performance benchmark tests.
	Evict(now time.Time)
}
