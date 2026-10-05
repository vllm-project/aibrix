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
	"math"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
)

const (
	MovingInterval        = 10 * time.Second
	MaxOutputLen          = 4096 // TODO: override this value if profile is provided.
	DefaultColdPrediction = OptimisticColdPrediction
)

const (
	// OptimisticColdPrediction predicts the output to be minimum 1 to be profile friendly. (most profiles should best result if output length is minimum)
	OptimisticColdPrediction ColdPredictionStrategy = iota
	// RandomColdPredition randomly predicts the output between 0 and maxOutputTokens.
	RandomColdPredition
	// InputColdPrediction predicts the output to be the same as the input.
	InputColdPrediction
	// PessimiticColdPrediction predicts the output to be maximum maxOutputTokens.
	PessimiticColdPrediction
)

// ColdPredictionStrategy defines the strategy when there is no history for the predictor.
type ColdPredictionStrategy int

// SimpleOutputPredictor collects moving histogram of output tokens of completed requests corresponding each input token buckets,
// and uses weighted random to predict output tokens for a specific request.
// Usage:
// 1. NewSimpleOutputPredictor() with max input and output estimation, specifying the window size.
// 2. AddTrace() to collect seen output tokens. Output tokens will be categorized in input token bucket of round(log2(input tokens)).
// 3. Call Predict() to get a prediction of number of output tokens by the number of input tokens.
type SimpleOutputPredictor struct {
	history       rotatingHistory
	inputs        outputDistribution
	inputsSums    []int32
	inputBuckets  int
	outputBuckets int

	mu       sync.RWMutex
	rand     func(int32) int32
	testing  bool
	testWait sync.WaitGroup
}

// Inputs/Output distribution
type outputDistribution []int32

func (hist outputDistribution) reset(distributions outputDistribution, sums []int32, outputBuckets int) {
	inputBucket := 0
	leftOutputBucket := outputBuckets
	var i int
	for i = 0; i < len(hist)-1; i++ {
		atomic.AddInt32(&sums[inputBucket], -hist[i])
		atomic.AddInt32(&distributions[i], -hist[i])
		hist[i] = 0
		leftOutputBucket--
		if leftOutputBucket == 0 {
			inputBucket++
			leftOutputBucket = outputBuckets
		}
	}
	// Reset skip slot
	hist[i] = 0
}

func (hist outputDistribution) setSkipped(skipped int32) {
	hist[len(hist)-1] = skipped
}

func (hist outputDistribution) getSkipped() int32 {
	return hist[len(hist)-1]
}

type rotatingHistory struct {
	window        []outputDistribution // input tokens * output tokens + 1 (skip slot)
	head          int
	tail          int
	headTimestamp time.Time
	// History size of the window. The head itself is not counted.
	// The size could be larger than window size due to skipping
	size int32
}

func (hist *rotatingHistory) Tail() outputDistribution {
	return hist.window[hist.tail]
}

func (hist *rotatingHistory) Head() outputDistribution {
	return hist.window[hist.head]
}

func (hist *rotatingHistory) Size() int32 {
	return atomic.LoadInt32(&hist.size)
}

func (hist *rotatingHistory) forwardLocked(ts time.Time) int32 {
	if ts.Sub(hist.headTimestamp) < MovingInterval {
		return 0
	}

	forwarded := int32(0)
	newHeadTimestamp := hist.headTimestamp
	for ts.Sub(newHeadTimestamp) >= MovingInterval {
		forwarded++
		newHeadTimestamp = newHeadTimestamp.Add(MovingInterval)
	}
	// Assert: new head is reset.
	hist.head = (hist.head + 1) % len(hist.window)
	hist.headTimestamp = newHeadTimestamp
	hist.window[hist.head].setSkipped(forwarded)
	atomic.AddInt32(&hist.size, forwarded)

	return forwarded
}

func (hist *rotatingHistory) resetTail(distributions outputDistribution, sums []int32, outputBuckets int) {
	hist.Tail().reset(distributions, sums, outputBuckets)
	hist.tail = (hist.tail + 1) % len(hist.window)
	atomic.AddInt32(&hist.size, -hist.Tail().getSkipped())
}

func NewSimpleOutputPredictor(maxInputTokens, maxOutputTokens int, window time.Duration) *SimpleOutputPredictor {
	// We allocate 1 more history slot to make summary update on rotating lock free
	extraSlot := 1
	if window%MovingInterval > 0 {
		extraSlot++
	}
	inputBuckets := int(math.Ceil(math.Log2(float64(maxInputTokens + 1))))
	outputBuckets := int(math.Ceil(math.Log2(float64(maxOutputTokens + 1))))
	predictor := &SimpleOutputPredictor{
		history: rotatingHistory{
			window:        make([]outputDistribution, int(window/MovingInterval)+extraSlot),
			headTimestamp: time.Now(),
		},
		inputs:        make(outputDistribution, inputBuckets*outputBuckets),
		inputsSums:    make([]int32, inputBuckets),
		inputBuckets:  inputBuckets,
		outputBuckets: outputBuckets,
		rand:          rand.Int31n,
	}
	for i := 0; i < len(predictor.history.window); i++ {
		predictor.history.window[i] = make(outputDistribution, inputBuckets*outputBuckets+1)
	}
	return predictor
}

func (p *SimpleOutputPredictor) AddTraceWithTimestamp(inputTokens, outputTokens int, cnt int32, ts time.Time) {
	p.tryRotate(ts)

	inputBucket := p.token2bucket(inputTokens, p.inputBuckets)
	idx := p.bucket2idx(inputBucket, p.token2bucket(outputTokens, p.outputBuckets))

	// In testing, enforce time series
	if p.testing {
		p.testWait.Wait()
	}

	// Avoid operations during rotating
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Add summary first and history next to avoid possible negative summary on rotating.
	atomic.AddInt32(&p.inputs[idx], cnt)
	atomic.AddInt32(&p.inputsSums[inputBucket], cnt)
	atomic.AddInt32(&p.history.window[p.history.head][idx], cnt)
}

func (p *SimpleOutputPredictor) AddTrace(inputTokens, outputTokens int, cnt int32) {
	p.AddTraceWithTimestamp(inputTokens, outputTokens, cnt, time.Now())
}

func (p *SimpleOutputPredictor) Predict(inputTokens int) int {
	inputBucket := p.token2bucket(inputTokens, p.inputBuckets)
	randRange := atomic.LoadInt32(&p.inputsSums[inputBucket])
	if randRange == int32(0) {
		return p.coldPredict(inputTokens) // No history, return input tokens
	}
	// Do weighted random
	cursor := p.rand(randRange)
	accumulation := int32(0)
	scanRange := (inputBucket + 1) * p.outputBuckets
	for i := scanRange - p.outputBuckets; i < scanRange; i++ {
		accumulation += atomic.LoadInt32(&p.inputs[i])
		if cursor < accumulation {
			return int(math.Pow(2, float64(i-scanRange+p.outputBuckets)))
		}
	}
	return int(math.Pow(2, float64(p.outputBuckets-1)))
}

func (p *SimpleOutputPredictor) coldPredict(inputTokens int) int {
	switch DefaultColdPrediction {
	case RandomColdPredition:
		return rand.Intn(MaxOutputLen) + 1
	case InputColdPrediction:
		return inputTokens
	case PessimiticColdPrediction:
		return MaxOutputLen
	default:
		// Default to optimistic
		return 1
	}
}

func (p *SimpleOutputPredictor) bucket2idx(inputBucket, outputBucket int) int {
	return inputBucket*p.outputBuckets + outputBucket
}

func (p *SimpleOutputPredictor) token2bucket(tokens int, limit int) int {
	bucket := 0
	if tokens > 0 {
		bucket = int(math.Round(math.Log2(float64(tokens))))
	}
	if bucket >= limit {
		bucket = limit - 1
	}
	return bucket
}

func (p *SimpleOutputPredictor) tryRotate(ts time.Time) {
	if ts.Sub(p.history.headTimestamp) < MovingInterval {
		return
	}
	if p.testing {
		p.testWait.Add(1)
	}
	go p.rotate(ts)
	runtime.Gosched() // allow rotate first.
}

func (p *SimpleOutputPredictor) rotate(ts time.Time) bool {
	if p.testing {
		defer p.testWait.Done()
	}

	window := int32(len(p.history.window) - 1)
	if p.history.Size() > window {
		klog.Error("unexpected no spare time slot in SimpleOutputPredictor")
		return false
	}

	// log.Printf("size %d", p.history.size)
	p.mu.Lock()
	defer p.mu.Unlock()

	// Calculate how many intervals we need to forward.
	// This is usually 1, for sparse workloads, this can be > 1.
	if p.history.forwardLocked(ts) == 0 {
		// Already forwarded
		return true
	}

	// Remove olded data from summary and reset history of number min(forwarded, len(p.history.window) - 1)
	// Noted that the
	// 1. read window size is len(p.history.window) - 1
	// 2. history.Size() should keep smaller than window because Head is not counted.
	for p.history.Size() >= window {
		p.history.resetTail(p.inputs, p.inputsSums, p.outputBuckets)
	}
	return true
}
