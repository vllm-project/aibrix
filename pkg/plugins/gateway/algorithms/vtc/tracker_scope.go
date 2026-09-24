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

package vtc

import (
	"sync"

	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/types"
)

// trackerScope is the identity of a token tracker a model config profile scoped
// to itself: the resolved window, time unit and token floors, plus the two
// token weights. All six are baked into the tracker instance rather than read
// per request, so a request whose profile resolves them to the process
// defaults shares the process-wide tracker of its router, while a request that
// changes any of them gets a tracker of its own.
type trackerScope struct {
	knobs        types.VTCTokenTrackerOverrides
	inputWeight  float64
	outputWeight float64
}

// maxTrackerScopes bounds how many distinct trackers one gateway process keeps.
// Beyond it a request keeps the process-wide tracker and therefore the
// environment values, which is the behaviour it would have had without the
// profile override: scoped state must not be able to grow without bound, and a
// request must never fail because too many profiles asked for their own
// tracker.
const maxTrackerScopes = 16

// trackerRegistry hands out one tracker per scope, building it on first use.
type trackerRegistry struct {
	mu     sync.Mutex
	scopes map[trackerScope]TokenTracker
	full   bool
}

// get returns the tracker of scope and whether the registry could keep one.
// The caller keeps the process-wide tracker when it reports false, so the
// request still routes with the environment values instead of failing.
func (r *trackerRegistry) get(scope trackerScope, build func() TokenTracker) (TokenTracker, bool) {
	r.mu.Lock()
	if tracker, ok := r.scopes[scope]; ok {
		r.mu.Unlock()
		return tracker, true
	}
	if len(r.scopes) >= maxTrackerScopes {
		r.noteFullLocked()
		r.mu.Unlock()
		return nil, false
	}
	r.mu.Unlock()

	// Build outside the lock: two requests of the same profile can race here,
	// and the loser keeps the winner's tracker so the profile still ends up
	// with exactly one.
	tracker := build()

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.scopes[scope]; ok {
		return existing, true
	}
	if len(r.scopes) >= maxTrackerScopes {
		r.noteFullLocked()
		return nil, false
	}
	if r.scopes == nil {
		r.scopes = make(map[trackerScope]TokenTracker)
	}
	r.scopes[scope] = tracker
	return tracker, true
}

// noteFullLocked logs the scope limit once per registry. The caller holds r.mu.
func (r *trackerRegistry) noteFullLocked() {
	if r.full {
		return
	}
	r.full = true
	klog.Warningf("more than %d model config profiles asked for their own VTC token tracker; further profiles keep the process-wide tracker and its environment values", maxTrackerScopes)
}
