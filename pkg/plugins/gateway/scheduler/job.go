/*
Copyright 2025 The Aibrix Team.

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

package scheduler

import (
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
)

// SchedulingJob represents a request waiting in the priority queue.
// It holds all context needed for scheduling and final execution.
type SchedulingJob struct {
	SessionID      string
	RequestContext *types.RoutingContext

	// Priority-related fields, populated on submission.
	InheritedCST  time.Duration
	TotalWaitTime time.Duration

	// Timestamps for aging and starvation detection.
	SubmissionTime time.Time

	// ResultChan is used by the scheduling loop to send the decision back.
	ResultChan chan *Decision

	// for heap.Interface
	index int
}

const (
	STARVATION_THRESHOLD = 2.0
)

// PriorityQueue implements heap.Interface for SchedulingJob pointers.
// -----------------------------------------------------------------------------------
// In Autellix paper, they use MLFQ instead of a single priority queue.
// -----------------------------------------------------------------------------------
//   - Avoiding thrashing: If priorities are sequential,
//     a request with a priority of 100.0 might be preempted by a new request
//     with a priority of 99.9.
//     This extremely small difference in priority can lead to preemption,
//     resulting in context switching overhead (KV cache swap) that
//     far outweighs the benefits of preemption.
//     This can cause system thrashing.
//   - Avoiding worst-case scenarios: The paper notes that in some cases,
//     the performance of preemptive scheduling with sequential priorities can degrade,
//     even worse than simple FCFS.
//   - Batch-friendly: Discretizing priorities into several queues
//     makes it natural to batch requests within each queue.
//
// -----------------------------------------------------------------------------------
// HOWEVER, I plan to use a single priority queue for the following reasons:
//   - We perform request-level scheduling at the Gateway layer.
//     We do not interrupt requests already executing on a Pod.
//     Our "preemption" simply determines which Pod should be sent the next request.
//     In this model, the context switch cost is zero.
//   - The implementation of a single Heap is much simpler and easier to maintain.
//   - MLFQ requires you to predefine the number of queues and the priority range
//     for each queue (for example, queues with a CST of 0-1 seconds go to Q1...).
//     This requires fine-tuning and is not very flexible.

type PriorityQueue []*SchedulingJob

func (pq PriorityQueue) Len() int { return len(pq) }

// Less is the core of the priority logic, including anti-starvation.
func (pq PriorityQueue) Less(i, j int) bool {

	// Calculate effective priority for item i
	priority_i := float64(pq[i].InheritedCST)
	// Add a small epsilon to avoid division by zero
	if priority_i > 1 && float64(pq[i].TotalWaitTime)/(priority_i+1e-9) > STARVATION_THRESHOLD {
		priority_i *= 0.1 // Apply a significant priority boost
	}

	// Calculate effective priority for item j
	priority_j := float64(pq[j].InheritedCST)
	if priority_j > 1 && float64(pq[j].TotalWaitTime)/(priority_j+1e-9) > STARVATION_THRESHOLD {
		priority_j *= 0.1
	}

	// The smaller the effective priority, the higher the actual priority
	return priority_i < priority_j
}

func (pq PriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *PriorityQueue) Push(x any) {
	job := x.(*SchedulingJob)
	job.index = len(*pq)
	*pq = append(*pq, job)
}

func (pq *PriorityQueue) Pop() any {
	old := *pq
	n := len(old)
	job := old[n-1]
	old[n-1] = nil // avoid memory leak
	job.index = -1
	*pq = old[0 : n-1]
	return job
}
