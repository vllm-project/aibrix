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

package impl

import (
	"context"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	"github.com/vllm-project/aibrix/apps/console/api/planner/utils"
	"k8s.io/klog/v2"
)

// planningLoop runs the planning loop.
type planningLoop struct {
	trigger      chan struct{}
	planInterval time.Duration
	planner      *Planner
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup // tracks goroutine completion
	ready        chan struct{}  // signaled when goroutine is ready to receive triggers
	isRunning    atomic.Bool
	// workerPool executes queue processing functions submitted by planningLoop
	workerPool *utils.WorkerPool
}

// newPlanningLoop creates a new planning worker.
func newPlanningLoop(planner *Planner, interval time.Duration, workerCount int) *planningLoop {
	return &planningLoop{
		trigger:      make(chan struct{}, 1),
		planInterval: interval,
		planner:      planner,
		ready:        make(chan struct{}),
		workerPool:   utils.NewWorkerPool(workerCount),
	}
}

// Trigger triggers an immediate planning cycle (non-blocking).
func (w *planningLoop) Trigger() {
	select {
	case w.trigger <- struct{}{}:
	default:
		// Already a pending trigger
	}
}

// Start starts the planning worker and waits for the goroutine to be ready.
func (w *planningLoop) Start(ctx context.Context) {
	if w.isRunning.Load() {
		return
	}
	w.isRunning.Store(true)
	w.ctx, w.cancel = context.WithCancel(ctx)
	w.wg.Add(1)
	w.workerPool.Start(w.ctx)
	go w.runWithTrigger()
	// Wait for goroutine to be ready to receive triggers
	<-w.ready
}

// Stop stops the planning worker and waits for the goroutine to exit
func (w *planningLoop) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if w.workerPool != nil {
		w.workerPool.Stop()
	}
	w.wg.Wait() // Wait for goroutine to exit
	w.isRunning.Store(false)
}

func (w *planningLoop) runWithTrigger() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.planInterval)
	defer ticker.Stop()

	// Signal that we're ready to receive triggers
	close(w.ready)

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.trigger:
			w.planOnce()
		case <-ticker.C:
			w.planOnce()
		}
	}
}

func (w *planningLoop) planOnce() {
	// 1. Remove terminal jobs
	w.removeTerminalJobs()

	// 2. Run policy
	err := w.planner.policy.Plan(w.ctx, PlanningInput[*queuedJob]{
		PlannerBackend: w.planner.backend,
		RunningQueue:   w.planner.runningQueue,
		PendingQueue:   w.planner.pendingQueue,
	})
	if err != nil {
		klog.Warningf("planning: policy failed: %v", err)
		return
	}

	// 3. Collect jobs from each queue and submit individual tasks
	w.processPendingQueue()
	w.processRunningQueue()
}

func (w *planningLoop) processPendingQueue() {
	p := w.planner

	// Collect pending jobs
	var pendingCancelling []*queuedJob
	var toProvision []*queuedJob
	p.pendingQueue.ForEach(func(job *queuedJob) bool {
		job.mu.RLock()
		status := job.status
		isScheduled := job.scheduledResource != nil
		job.mu.RUnlock()
		switch status {
		case plannerapi.JobStatusCancelling:
			pendingCancelling = append(pendingCancelling, job)
		case plannerapi.JobStatusQueued:
			// Only process if planned and no provisionID yet
			if isScheduled {
				toProvision = append(toProvision, job)
			}
		}
		return true
	})

	for _, job := range pendingCancelling {
		handleCleanup(w.planner, job, plannerapi.JobStatusCancelling, plannerapi.JobStatusCancelled)
	}

	// Execute provisioning
	for _, job := range toProvision {
		handleProvisioning(p, job)
	}
}

func (w *planningLoop) processRunningQueue() {
	wp := w.workerPool
	p := w.planner

	// First, submit ready jobs to MDS
	var toSubmit []*queuedJob
	p.runningQueue.ForEach(func(job *queuedJob) bool {
		job.mu.RLock()
		status := job.status
		readyToSubmit := job.readyToSubmit
		job.mu.RUnlock()

		if status == plannerapi.JobStatusResourcePreparing && readyToSubmit {
			toSubmit = append(toSubmit, job)
		}
		return true
	})

	for _, job := range toSubmit {
		submitToMDS(p, job)
	}

	// Then, dispatch query-only operations to worker pool
	p.runningQueue.ForEach(func(job *queuedJob) bool {
		job.mu.RLock()
		status := job.status
		if status.IsTerminal() {
			job.mu.RUnlock()
			return true
		}
		deadline := job.expiresAt
		job.mu.RUnlock()

		if !deadline.IsZero() && deadline.Before(time.Now().UTC()) {
			// Job expired
			wp.Submit(func() { handleCleanup(w.planner, job, status, plannerapi.JobStatusExpired) })
			return true
		}

		switch status {
		case plannerapi.JobStatusCancelling:
			wp.Submit(func() { handleCleanup(w.planner, job, status, plannerapi.JobStatusCancelled) })
		case plannerapi.JobStatusResourcePreparing:
			// Query provision status only, mark readyToSubmit if ready
			wp.Submit(func() { handleResourcePreparing(w.planner, job) })
		default:
			if isBatchRunning(status) {
				// Query batch status only, update job state
				wp.Submit(func() { handleRunning(w.planner, job) })
			}
			// skip others
		}
		return true
	})
}

// removeTerminalJobs removes jobs that have failed
func (w *planningLoop) removeTerminalJobs() {
	w.planner.mu.Lock()
	jobsClone := maps.Clone(w.planner.jobs)
	w.planner.mu.Unlock()

	keysToRemove := make(map[utils.PriorityQueue[*queuedJob]][]string)
	toDelete := make([]string, 0)
	for jobID, job := range jobsClone {
		job.mu.Lock()
		status := job.status
		queue := job.queue
		job.queue = nil
		job.mu.Unlock()
		if status.IsTerminal() {
			toDelete = append(toDelete, jobID)
			if queue != nil {
				keysToRemove[queue] = append(keysToRemove[queue], jobID)
			}
		}
	}

	w.planner.mu.Lock()
	for _, jobID := range toDelete {
		delete(w.planner.jobs, jobID)
	}
	w.planner.mu.Unlock()

	for queue, keys := range keysToRemove {
		queue.Remove(keys...)
	}
}
