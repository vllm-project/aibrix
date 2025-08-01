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
	"container/heap"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/scheduler/sessioninfo"
	"github.com/vllm-project/aibrix/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// inProcessScheduler is the in-memory, in-process implementation of the Scheduler interface.
type inProcessScheduler struct {
	queue        PriorityQueue
	mu           sync.Mutex
	sessionCache *sessioninfo.MutexSessionCache
	newJobCond   *sync.Cond
	stopChan     chan struct{}
	stopFunc     func()               // Function to stop the cleanup routine
	k8sClient    kubernetes.Interface // For fetching pod state
}

// NewScheduler creates and starts a new scheduler.
func NewScheduler(k8sClient kubernetes.Interface) Scheduler {
	cache := sessioninfo.NewMutexSessionCache()
	// Start the cleanup routine and get its stop function
	stopCleanup := cache.StartCleanupRoutine(1*time.Minute, 10*time.Minute)

	s := &inProcessScheduler{
		sessionCache: cache,
		stopChan:     make(chan struct{}),
		stopFunc:     stopCleanup,
		k8sClient:    k8sClient,
	}
	s.newJobCond = sync.NewCond(&s.mu)
	heap.Init(&s.queue)
	go s.schedulingLoop()
	return s
}

func (s *inProcessScheduler) SubmitJob(ctx *types.RoutingContext, sessionID string) (*Decision, error) {
	cst, waitTime := s.sessionCache.GetOrCreateForScheduler(sessionID)

	job := &SchedulingJob{
		SessionID:      sessionID,
		RequestContext: ctx,
		InheritedCST:   cst,
		TotalWaitTime:  waitTime,
		SubmissionTime: time.Now(),
		ResultChan:     make(chan *Decision, 1),
	}

	s.mu.Lock()
	heap.Push(&s.queue, job)
	s.mu.Unlock()
	s.newJobCond.Signal()

	decision := <-job.ResultChan
	return decision, decision.Err
}

func (s *inProcessScheduler) FinalizeJob(sessionID string, inheritedCST, executionTime, waitTime time.Duration) {
	s.sessionCache.UpdateState(sessionID, inheritedCST, executionTime, waitTime)
}

func (s *inProcessScheduler) UpdateAffinity(sessionID, podName string) {
	s.sessionCache.UpdateAffinity(sessionID, podName)
}

func (s *inProcessScheduler) Stop() {
	s.stopFunc() // Stop the cache cleanup routine
	close(s.stopChan)
	s.newJobCond.Broadcast()
}

func (s *inProcessScheduler) schedulingLoop() {
	for {
		s.mu.Lock()
		for len(s.queue) == 0 {
			select {
			case <-s.stopChan:
				s.mu.Unlock()
				return
			default:
			}
			s.newJobCond.Wait()
		}

		// Simplified: schedule one job at a time.
		// TODO: multi-job at one time
		job := heap.Pop(&s.queue).(*SchedulingJob)
		s.mu.Unlock()

		// The scheduler's only job is to decide WHO is next.
		// It does not decide WHERE it goes. That's the gateway's router's job.
		// So we just send the job back as the decision.
		decision := &Decision{
			Job: job,
			Err: nil,
		}

		job.ResultChan <- decision
	}
}
