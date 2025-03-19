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
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
)

const (
	expireWriteRequestTraceIntervalInMins = 10
)

func (c *Store) getRequestTrace(modelName string) *RequestTrace {
	trace := NewRequestTrace(time.Now().UnixNano())
	newer, loaded := c.requestTrace.LoadOrStore(modelName, trace)
	if loaded {
		trace.Recycle()
	} else {
		atomic.AddInt32(&c.numRequestsTraces, 1)
	}
	return newer.(*RequestTrace)
}

func (c *Store) getTraceKey(inputTokens, outputTokens int64) (traceKey string) {
	if inputTokens > 0 && outputTokens > 0 {
		inputIndex := int64(math.Round(math.Log2(float64(inputTokens)) / RequestTracePrecision)) // Round to the nearest precision and convert to int
		outputIndex := int64(math.Round(math.Log2(float64(outputTokens)) / RequestTracePrecision))
		traceKey = fmt.Sprintf("%v:%v", inputIndex, outputIndex)

		klog.V(5).Infof("inputTokens: %v, inputIndex: %v, outputTokens: %v, outputIndex: %v",
			inputTokens, inputIndex, outputTokens, outputIndex)
	}
	return
}

func (c *Store) writeRequestTraceToStorage(roundT int64) {
	// Save and reset trace context, atomicity is guaranteed.
	var requestTrace *sync.Map
	numTraces := atomic.LoadInt32(&c.numRequestsTraces)
	requestTrace, c.requestTrace = c.requestTrace, &sync.Map{}
	numResetTo := int32(0)
	// TODO: Adding a unit test here.
	for !atomic.CompareAndSwapInt32(&c.numRequestsTraces, numTraces, numResetTo) {
		// If new traces added to reset map, assert updatedNumTraces >= numTraces regardless duplication.
		updatedNumTraces := atomic.LoadInt32(&c.numRequestsTraces)
		numTraces, numResetTo = updatedNumTraces, updatedNumTraces-numTraces
	}

	requestTrace.Range(func(iModelName, iTrace any) bool {
		modelName := iModelName.(string)
		trace := iTrace.(*RequestTrace)
		requestTrace.Store(modelName, nil) // Simply assign nil instead of delete

		trace.Lock()
		pending := int32(0)
		if pCounter, loaded := c.pendingRequests.Load(modelName); loaded {
			pending = atomic.LoadInt32(pCounter.(*int32))
		}
		traceMap := trace.ToMapLocked(pending)
		trace.RecycleLocked()
		trace.Unlock()

		value, err := json.Marshal(traceMap)
		if err != nil {
			klog.ErrorS(err, "error to marshall request trace for redis set")
			return true
		}

		key := fmt.Sprintf("aibrix:%v_request_trace_%v", modelName, roundT)
		if _, err = c.redisClient.Set(context.Background(), key, value, expireWriteRequestTraceIntervalInMins*time.Minute).Result(); err != nil {
			klog.Error(err)
		}
		return true
	})

	klog.V(5).Infof("writeRequestTraceWithKey: %v", roundT)
}
