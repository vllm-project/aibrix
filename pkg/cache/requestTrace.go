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
	MetaKeyPendingRequests // Pending requests that out of trace window
	RequestTraceNumMetaKeys
)

var requestTraceMetaKeys = [...]string{"meta_v", "meta_interval_sec", "meta_precision", "meta_total_reqs", "meta_pending_reqs", "meta_len"}

func (key RequestTraceMetaKey) ToString() string {
	return requestTraceMetaKeys[key]
}

const (
	// The version of request trace, verison history:
	// v1: No meta, default
	// v2: Added meta data include version(meta_v), bucket precision(meta_precision), and interval(meta_interval_sec) to notify client the trace interval.
	// v3: Added the number of total requests(meta_total_reqs) and pending requests(meta_pending_reqs) for out of window uncomplted requests.
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
	trace           *sync.Map // map[Log2(input_token):Log2(output_token)]request_count
	numKeys         int32     // The number of keys in the trace.
	numRequests     int32     // Total requests seen in the trace window
	pendingRequests int32     // Total pending requests remain in the trace window

	mu       sync.RWMutex
	recycler func(any) // Function handler to put RequestTrace back to pool.
}

// Increase request counting and return the trace term, key is ignored for now.
func (t *RequestTrace) AddRequest(requestID string, key string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// Check if recycled
	if t.recycler == nil {
		return false
	}
	atomic.AddInt32(&t.numRequests, 1)
	atomic.AddInt32(&t.pendingRequests, 1)
	return true
}

// Decrease request counting with term verification
func (t *RequestTrace) DoneRequest(requestID string, key string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// Check if recycled
	if t.recycler == nil {
		return false
	}

	// TODO: Check request membership using requestID
	atomic.AddInt32(&t.pendingRequests, -1)

	counter := int32(1)
	if pCounter, loaded := t.trace.LoadOrStore(key, &counter); loaded {
		atomic.AddInt32(pCounter.(*int32), 1)
	} else {
		atomic.AddInt32(&t.numKeys, 1)
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
	// pendingRequests should not be negative even without membership checking.
	pendingRequests := atomic.LoadInt32(&t.pendingRequests)
	if pendingRequests < 0 {
		pendingRequests = 0
	}
	ret[MetaKeyPendingRequests.ToString()] = int(total_pending - pendingRequests)
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

// Get a RequestTrace generator by hidding the tracePool in closure. Do not call this directly unless for testing purpose.
func newRequestTraceGen(tracePool *sync.Pool) func() *RequestTrace {
	if tracePool == nil {
		tracePool = &sync.Pool{}
	}
	recycler := tracePool.Put
	tracePool.New = func() any { return &RequestTrace{trace: &sync.Map{}, recycler: recycler} }
	return func() *RequestTrace {
		reqTrace := tracePool.Get().(*RequestTrace)
		if atomic.LoadInt32(&reqTrace.numKeys) > 0 {
			reqTrace.trace = &sync.Map{}
			atomic.StoreInt32(&reqTrace.numKeys, 0)
		}
		atomic.StoreInt32(&reqTrace.numRequests, 0)
		atomic.StoreInt32(&reqTrace.pendingRequests, 0)
		reqTrace.recycler = recycler
		return reqTrace
	}
}

var NewRequestTrace = newRequestTraceGen(nil)
