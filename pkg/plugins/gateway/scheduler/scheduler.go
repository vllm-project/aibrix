package scheduler

import (
	"container/heap"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/scheduler/sessioninfo"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// inProcessScheduler implements a high-throughput, low-latency scheduler.
// It uses a lock-free channel for job submission and a single goroutine
// for all stateful processing to eliminate lock contention on the hot path.
type inProcessScheduler struct {
	// A buffered channel for lock-free submission from multiple goroutines.
	// This is the main ingress point for new requests.
	submitChan chan *SchedulingJob

	// The session cache, which is thread-safe itself.
	sessionCache *sessioninfo.MutexSessionCache

	// A channel to signal all background goroutines to stop gracefully.
	stopChan chan struct{}

	// Kubernetes client to get information about the cluster state.
	k8sClient kubernetes.Interface

	// --- Dynamic Batching Fields ---
	// A thread-safe container for the list of currently healthy and routable pods.
	// Updated periodically by the podWatcherLoop.
	healthyPods atomic.Value
	// A thread-safe counter for requests that have been scheduled but not yet finalized.
	// Used to calculate available capacity.
	inflightRequests atomic.Int64
}

const (
	// The size of the channel buffer for job submission.
	// This should be large enough to absorb bursts of requests.
	SUBMIT_CHAN_BUFFER_SIZE = 1024
	// The interval at which the processing loop checks for new jobs and tries to schedule them.
	PROCESSING_LOOP_INTERVAL = 5 * time.Millisecond
)

// NewScheduler creates and starts the new high-throughput scheduler.
func NewScheduler(k8sClient kubernetes.Interface, cache *sessioninfo.MutexSessionCache) Scheduler {
	s := &inProcessScheduler{
		// A large buffer can absorb bursts of requests without blocking SubmitJob.
		submitChan:   make(chan *SchedulingJob, SUBMIT_CHAN_BUFFER_SIZE),
		sessionCache: cache,
		stopChan:     make(chan struct{}),
		k8sClient:    k8sClient,
	}

	// Initialize atomic values with empty/zero state.
	s.healthyPods.Store([]*v1.Pod{})
	s.inflightRequests.Store(0)

	// Start the single, powerful processing loop.
	go s.processingLoop()
	// Start background goroutine for pod watching.
	go s.podWatcherLoop()

	return s
}

// podWatcherLoop periodically queries the k8s API server to get an updated
// list of healthy pods, which is used to determine the cluster's processing capacity.
func (s *inProcessScheduler) podWatcherLoop() {
	// A ticker to trigger the pod list refresh at regular intervals.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			// In a real implementation, this would list pods with a specific label selector.
			// For example:
			// podList, err := s.k8sClient.CoreV1().Pods("aibrix-system").List(context.TODO(), metav1.ListOptions{LabelSelector: "app=llm-server"})
			// if err != nil {
			// 	 klog.Errorf("Scheduler: failed to list pods: %v", err)
			// 	 continue
			// }
			//
			// readyPods := filterReadyPods(podList.Items)
			// s.healthyPods.Store(readyPods)
		}
	}
}

// SubmitJob is now extremely lightweight and lock-free.
// It simply sends the job to a channel and immediately returns a channel
// on which the caller can wait for the final decision.
func (s *inProcessScheduler) SubmitJob(ctx *types.RoutingContext, sessionID string) (*Decision, error) {
	// The job no longer contains state from the cache.
	// State enrichment happens inside the processingLoop.
	job := &SchedulingJob{
		SessionID:      sessionID,
		RequestContext: ctx,
		SubmissionTime: time.Now(),
		ResultChan:     make(chan *Decision, 1),
	}

	// This send operation is thread-safe and highly optimized.
	// It will only block if the channel buffer is full, which acts as a natural backpressure mechanism.
	s.submitChan <- job

	// The goroutine now blocks waiting for the decision, NOT on a lock.
	decision := <-job.ResultChan
	if decision.Err == nil {
		s.inflightRequests.Add(1)
	}
	return decision, decision.Err
}

// FinalizeJob is also made asynchronous by sending a message.
// We can use a different type or a special field in SchedulingJob to signify finalization.
func (s *inProcessScheduler) FinalizeJob(sessionID string, inheritedCST, executionTime, waitTime time.Duration) {
	s.inflightRequests.Add(-1)
	// The state update is still directly on the cache, which is thread-safe.
	// This is fine because it doesn't contend with the scheduling logic.
	s.sessionCache.UpdateState(sessionID, inheritedCST, executionTime, waitTime)

	// A crucial step: a finished job might have freed up capacity.
	// We need to trigger the scheduling loop to check again.
	// We can send a nil job as a signal.
	select {
	case s.submitChan <- nil: // Send a special signal to wake up the loop
	default: // Don't block if the channel is full; the loop is already busy.
	}
}

func (s *inProcessScheduler) Stop() {
	close(s.stopChan)
}

// processingLoop is the single-threaded owner of the priority queue.
// This eliminates ALL lock contention for scheduling logic.
func (s *inProcessScheduler) processingLoop() {
	// The priority queue is now a local variable, not shared.
	priorityQueue := make(PriorityQueue, 0)
	heap.Init(&priorityQueue)

	// Ticker for periodic, throughput-oriented scheduling.
	ticker := time.NewTicker(PROCESSING_LOOP_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopChan:
			return

		case job := <-s.submitChan:
			if job == nil { // This is a wakeup signal from FinalizeJob
				// We might have new capacity, so try to schedule immediately.
				s.scheduleBatch(&priorityQueue)
				continue
			}

			// --- State Enrichment ---
			cst, waitTime := s.sessionCache.GetOrCreateForScheduler(job.SessionID)
			job.InheritedCST = cst
			job.TotalWaitTime = waitTime

			// --- Enqueue ---
			heap.Push(&priorityQueue, job)

			// After a new high-priority job arrives, we should try to schedule immediately
			// to minimize its latency, rather than waiting for the next tick.
			s.scheduleBatch(&priorityQueue)

		case <-ticker.C:
			// --- Periodic Scheduling ---
			// This ensures that even if no new jobs arrive, we still try to
			// clear the queue to maximize throughput.
			s.scheduleBatch(&priorityQueue)
		}
	}
}

// scheduleBatch is now a private method that operates on the local priority queue.
func (s *inProcessScheduler) scheduleBatch(pq *PriorityQueue) {
	if len(*pq) == 0 {
		return
	}

	// The batching logic remains the same: based on dynamic capacity.
	batchSize := s.calculateBatchSize()
	if batchSize <= 0 {
		return
	}

	numToPop := min(batchSize, len(*pq))
	if numToPop <= 0 {
		return
	}

	// Pop and dispatch concurrently.
	for i := 0; i < numToPop; i++ {
		job := heap.Pop(pq).(*SchedulingJob)
		decision := &Decision{
			Job: job,
			Err: nil,
		}
		job.ResultChan <- decision
	}
}

// calculateBatchSize determines how many new requests can be scheduled in this cycle.
func (s *inProcessScheduler) calculateBatchSize() int {
	// Load the list of healthy pods atomically.
	pods, _ := s.healthyPods.Load().([]*v1.Pod)

	// The cluster's total capacity is assumed to be the number of healthy pods.
	// This can be enhanced later to read a `max_concurrent_requests` annotation from each pod.
	capacity := len(pods)
	if capacity == 0 {
		// Fallback to a default capacity if pod watcher hasn't run or finds no pods.
		// This makes the scheduler resilient.
		capacity = 1
	}

	// Load the number of currently inflight requests atomically.
	inflight := s.inflightRequests.Load()

	// The available slots are the total capacity minus what's already running.
	availableSlots := capacity - int(inflight)
	if availableSlots < 0 {
		return 0
	}
	return availableSlots
}

// min is a simple utility function.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
