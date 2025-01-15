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
package cache

import (
	"sync"
	"sync/atomic"
	"time"
)

type RequestTraceMetaKey int

const (
	MetaKeyVersionKey RequestTraceMetaKey = iota
	MetaKeyIntervalInSeconds
	MetaKeyTracePrecision
	MetaKeyTotalRequests
	MetaKeyPendingRequests
	RequestTraceNumMetaKeys // Guardian for the number of RequestTraceMetaKey. This is not a actual meta key.
)

var requestTraceMetaKeys = [...]string{"meta_v", "meta_interval_sec", "meta_precision", "meta_total_reqs", "meta_pending_reqs", "meta_len"}

func (key RequestTraceMetaKey) ToString() string {
	return requestTraceMetaKeys[key]
}

const (
	// The version of request trace, version history:
	// v1: No meta, default
	// v2: Added meta data include version(meta_v), bucket precision(meta_precision), and interval(meta_interval_sec) to notify client the trace interval.
	// v3: Added the number of total requests(meta_total_reqs) and pending requests(meta_pending_reqs) for uncompleted requests.
	RequestTraceVersion = 3
	// Trace write interval
	RequestTraceWriteInterval = 10 * time.Second
	// Max tolerable write delay to write ticks.
	// For example for RequestTraceWriteInterval = 10s and MaxRequestTraceIntervalOffset = 500ms, the trace should be written before X:00.5s, X:10.5s, .., X:50.5s.
	MaxRequestTraceIntervalOffset = 500 * time.Millisecond
	// The precision of buckets in trace. 0.1 means requests will be split into buckets of .1 according to log2(tokens)
	RequestTracePrecision = 0.1
)

type RequestTrace struct {
	trace             *sync.Map // map[Log2(input_token):Log2(output_token)]request_count
	numKeys           int32     // The number of keys in the trace.
	numRequests       int32     // Total requests seen in the trace window
	completedRequests int32     // Total completed requests remain in the trace window
	term              int64     // Term that identify the RequestTrace

	mu       sync.RWMutex
	recycler func(any) // Function handler to put RequestTrace back to pool.
}

// Increase request counting and return the trace term, key is ignored for now.
func (t *RequestTrace) AddRequest(requestID string, key string) (int64, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// Check if recycled
	if t.recycler == nil {
		return t.term, false
	}
	atomic.AddInt32(&t.numRequests, 1)
	return t.term, true
}

// Decrease request counting with term verification, retrying is fultile.
func (t *RequestTrace) DoneRequest(requestID string, term int64) {
	// No lock required, for the request is guarded by the term
	t.doneRequestLocked(term)
}

// Add request trace profile. key must be provided and will not be checked
func (t *RequestTrace) AddRequestTrace(requestID string, key string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// Check if recycled
	if t.recycler == nil {
		return false
	}

	t.addRequestTraceLocked(key)
	return true
}

// Decrease request counting and add request trace profile.
func (t *RequestTrace) DoneRequestTrace(requestID string, key string, term int64) bool {
	if term != t.term && key == "" {
		return true
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	// Check if recycled
	if t.recycler == nil {
		return false
	}

	t.doneRequestLocked(term)
	if key != "" {
		t.addRequestTraceLocked(key)
	}
	return true
}

func (t *RequestTrace) Lock() {
	t.mu.Lock()
}

func (t *RequestTrace) Unlock() {
	t.mu.Unlock()
}

func (t *RequestTrace) ToMapLocked(total_pending int32) map[string]int {
	ret := make(map[string]int, int(t.numKeys)+int(RequestTraceNumMetaKeys))
	t.trace.Range(func(_key, _count any) bool {
		ret[_key.(string)] = int(*(_count.(*int32)))
		return true
	})
	ret[MetaKeyVersionKey.ToString()] = RequestTraceVersion
	ret[MetaKeyIntervalInSeconds.ToString()] = int(RequestTraceWriteInterval / time.Second)
	ret[MetaKeyTracePrecision.ToString()] = int(1 / RequestTracePrecision)
	ret[MetaKeyTotalRequests.ToString()] = int(atomic.LoadInt32(&t.numRequests))
	ret[MetaKeyPendingRequests.ToString()] = int(total_pending) // Disregard differences between pending in or out of window in this version.
	return ret
}

func (t *RequestTrace) ToMap(total_pending int32) map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ToMapLocked(total_pending)
}

func (t *RequestTrace) RecycleLocked() {
	recycler := t.recycler
	t.recycler = nil
	recycler(t)
}

func (t *RequestTrace) Recycle() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.RecycleLocked()
}

func (t *RequestTrace) doneRequestLocked(term int64) {
	if term == t.term {
		atomic.AddInt32(&t.completedRequests, 1)
	}
}

func (t *RequestTrace) addRequestTraceLocked(key string) {
	// Init with 1 to avoid increasement on Store
	counter := int32(1)
	if pCounter, loaded := t.trace.LoadOrStore(key, &counter); loaded {
		// Increase counter correspondent to the key
		atomic.AddInt32(pCounter.(*int32), 1)
	} else {
		// Increase counter of total keys
		atomic.AddInt32(&t.numKeys, 1)
	}
}

// Get a RequestTrace generator by hidding the tracePool in closure. Do not call this directly unless for testing purpose.
func newRequestTraceGen(tracePool *sync.Pool) func(term int64) *RequestTrace {
	if tracePool == nil {
		tracePool = &sync.Pool{}
	}
	recycler := tracePool.Put
	tracePool.New = func() any { return &RequestTrace{trace: &sync.Map{}, recycler: recycler} }
	return func(term int64) *RequestTrace {
		reqTrace := tracePool.Get().(*RequestTrace)
		if atomic.LoadInt32(&reqTrace.numKeys) > 0 {
			reqTrace.trace = &sync.Map{}
			atomic.StoreInt32(&reqTrace.numKeys, 0)
		}
		atomic.StoreInt32(&reqTrace.numRequests, 0)
		atomic.StoreInt32(&reqTrace.completedRequests, 0)
		reqTrace.term = term
		reqTrace.recycler = recycler
		return reqTrace
	}
}

var NewRequestTrace = newRequestTraceGen(nil)
