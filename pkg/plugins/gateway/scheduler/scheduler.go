package scheduler

import (
	"container/heap"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/scheduler/sessioninfo"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
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

	// --- Load Awareness Integration ---
	// Cache for accessing real-time load metrics from Router
	cache cache.Cache
	// Load provider for advanced capacity calculation (reuses Router's logic)
	loadProvider cache.CappedLoadProvider

	// --- Batch Size Smoothing ---
	// Last calculated batch size for smoothing (atomic for thread safety)
	lastBatchSize atomic.Int32
}

const (
	// The size of the channel buffer for job submission.
	// This should be large enough to absorb bursts of requests.
	SUBMIT_CHAN_BUFFER_SIZE = 1024
	// The interval at which the processing loop checks for new jobs and tries to schedule them.
	PROCESSING_LOOP_INTERVAL = 5 * time.Millisecond

	// Default estimated concurrent capacity per pod when no annotation is provided
	DEFAULT_POD_CONCURRENT_CAPACITY = 1

	// Multiplier for estimating capacity from current running requests
	CAPACITY_ESTIMATION_MULTIPLIER = 2

	// Oversubscription factor - Router doesn't allow this, so we set to 1.0 (no oversubscription)
	SCHEDULER_OVERSUBSCRIPTION_FACTOR = 1.0

	// Batch size smoothing factor (exponential moving average alpha)
	BATCH_SIZE_SMOOTHING_ALPHA = 0.3

	// Pass-through mode batch size (essentially disables batching)
	PASS_THROUGH_BATCH_SIZE = 1000
)

// NewScheduler creates and starts the new high-throughput scheduler.
func NewScheduler(k8sClient kubernetes.Interface, sessionCache *sessioninfo.MutexSessionCache, cacheInstance cache.Cache) Scheduler {
	// Use provided cache instance for load awareness
	if cacheInstance == nil {
		klog.InfoS("no cache instance provided, falling back to basic scheduling")
	}

	// Initialize load provider for advanced capacity calculation
	var loadProvider cache.CappedLoadProvider
	if cacheInstance != nil {
		pendingLoadProvider, err := cache.NewPendingLoadProvider()
		if err != nil {
			klog.ErrorS(err, "failed to create PendingLoadProvider, using basic capacity calculation")
		} else {
			loadProvider = pendingLoadProvider
			klog.InfoS("scheduler initialized with advanced load awareness")
		}
	}

	s := &inProcessScheduler{
		// A large buffer can absorb bursts of requests without blocking SubmitJob.
		submitChan:   make(chan *SchedulingJob, SUBMIT_CHAN_BUFFER_SIZE),
		sessionCache: sessionCache,
		stopChan:     make(chan struct{}),
		k8sClient:    k8sClient,
		cache:        cacheInstance,
		loadProvider: loadProvider,
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
				// Just continue to the unified scheduling point
				break
			}

			// --- State Enrichment ---
			cst, waitTime := s.sessionCache.GetOrCreateForScheduler(job.SessionID)
			job.InheritedCST = cst
			job.TotalWaitTime = waitTime

			// --- Enqueue ---
			heap.Push(&priorityQueue, job)

		case <-ticker.C:
			// --- Periodic Scheduling ---
			// Just continue to the unified scheduling point
		}

		// --- Unified Scheduling Point ---
		// At the end of every loop iteration, regardless of what woke it up,
		// try to schedule a batch. This eliminates code duplication and
		// ensures consistent scheduling behavior.
		s.scheduleBatch(&priorityQueue)
	}
}

// scheduleBatch is now a private method that operates on the local priority queue.
func (s *inProcessScheduler) scheduleBatch(pq *PriorityQueue) {
	if len(*pq) == 0 {
		return
	}

	// The batching logic with smoothing: based on dynamic capacity with exponential moving average.
	batchSize := s.calculateSmoothedBatchSize()
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

// calculateSmoothedBatchSize determines how many new requests can be scheduled in this cycle.
// Uses exponential moving average to smooth out capacity fluctuations.
func (s *inProcessScheduler) calculateSmoothedBatchSize() int {
	rawBatchSize := s.calculateBatchSize()
	lastBatchSize := int(s.lastBatchSize.Load())

	var smoothed int
	if lastBatchSize == 0 {
		// First time or after reset, use raw value
		smoothed = rawBatchSize
	} else {
		// Apply exponential moving average smoothing
		// smoothed = (1-alpha) * last + alpha * current
		smoothed = int((1.0-BATCH_SIZE_SMOOTHING_ALPHA)*float64(lastBatchSize) +
			BATCH_SIZE_SMOOTHING_ALPHA*float64(rawBatchSize))
	}

	// Store the smoothed value for next iteration
	s.lastBatchSize.Store(int32(smoothed))

	klog.V(5).InfoS("batch size smoothing",
		"rawBatchSize", rawBatchSize,
		"lastBatchSize", lastBatchSize,
		"smoothedBatchSize", smoothed,
		"alpha", BATCH_SIZE_SMOOTHING_ALPHA)

	return smoothed
}

// calculateBatchSize determines how many new requests can be scheduled in this cycle.
// Now uses Router's load awareness for accurate capacity calculation.
func (s *inProcessScheduler) calculateBatchSize() int {
	// Load the list of healthy pods atomically.
	pods, _ := s.healthyPods.Load().([]*v1.Pod)
	if len(pods) == 0 {
		return 0
	}

	// Use advanced load awareness if available
	if s.cache != nil && s.loadProvider != nil {
		return s.calculateAdvancedBatchSize(pods)
	}

	// Improved fallback: use pod annotations even without cache
	return s.calculateImprovedFallbackBatchSize(pods)
}

// calculateAdvancedBatchSize uses Router's load awareness for precise capacity calculation
func (s *inProcessScheduler) calculateAdvancedBatchSize(pods []*v1.Pod) int {
	totalAvailableCapacity := 0

	for _, pod := range pods {
		// Get current utilization using Router's load provider
		utilization, err := s.loadProvider.GetUtilization(nil, pod)
		if err != nil {
			// If we can't get utilization, assume pod is available
			klog.V(4).InfoS("failed to get pod utilization, assuming available",
				"pod", pod.Name, "error", err)
			totalAvailableCapacity += 1
			continue
		}

		// Get pod's capacity limit (typically 1.0 for normalized load)
		podCapacity := s.loadProvider.Cap()

		// Calculate available capacity for this pod (following Router's strict capacity limits)
		availableCapacity := podCapacity - utilization
		if availableCapacity > 0 {
			// Convert normalized capacity to request slots
			// Use Router's approach: strict capacity limits without oversubscription
			estimatedConcurrentCapacity := s.getEstimatedConcurrentCapacity(pod)
			podAvailableSlots := int(availableCapacity * float64(estimatedConcurrentCapacity))
			totalAvailableCapacity += podAvailableSlots
		}

		klog.V(4).InfoS("pod capacity analysis",
			"pod", pod.Name,
			"utilization", utilization,
			"capacity", podCapacity,
			"availableCapacity", availableCapacity,
			"estimatedSlots", int(availableCapacity*float64(s.getEstimatedConcurrentCapacity(pod))))
	}

	// Apply scheduling strategy following Router's approach (no oversubscription)
	// Router strictly enforces capacity limits, so we follow the same principle
	finalCapacity := int(float64(totalAvailableCapacity) * SCHEDULER_OVERSUBSCRIPTION_FACTOR)

	klog.V(4).InfoS("advanced batch size calculation",
		"totalPods", len(pods),
		"totalAvailableCapacity", totalAvailableCapacity,
		"finalCapacity", finalCapacity,
		"oversubscriptionFactor", SCHEDULER_OVERSUBSCRIPTION_FACTOR)

	return max(0, finalCapacity)
}

// calculateImprovedFallbackBatchSize provides intelligent fallback without cache
func (s *inProcessScheduler) calculateImprovedFallbackBatchSize(pods []*v1.Pod) int {
	totalEstimatedCapacity := 0

	// Calculate total estimated capacity from pod annotations
	for _, pod := range pods {
		podCapacity := s.getEstimatedConcurrentCapacity(pod)
		totalEstimatedCapacity += podCapacity
	}

	// Calculate available capacity
	inflight := s.inflightRequests.Load()
	availableSlots := totalEstimatedCapacity - int(inflight)

	klog.V(4).InfoS("improved fallback batch size calculation",
		"totalPods", len(pods),
		"totalEstimatedCapacity", totalEstimatedCapacity,
		"inflight", inflight,
		"availableSlots", availableSlots)

	// If we still get very low capacity (indicating no annotations),
	// switch to pass-through mode
	if totalEstimatedCapacity <= len(pods) {
		klog.V(3).InfoS("switching to pass-through mode - no pod capacity annotations found",
			"totalEstimatedCapacity", totalEstimatedCapacity,
			"podCount", len(pods))
		// Return a large number to essentially disable batching
		// This allows requests to be routed immediately without scheduler bottleneck
		return PASS_THROUGH_BATCH_SIZE
	}

	return max(0, availableSlots)
}

// getEstimatedConcurrentCapacity estimates how many concurrent requests a pod can handle
func (s *inProcessScheduler) getEstimatedConcurrentCapacity(pod *v1.Pod) int {
	// Try to get from pod annotations first
	if pod.Annotations != nil {
		if maxConcurrency, exists := pod.Annotations["aibrix.io/max-concurrent-requests"]; exists {
			if val, err := strconv.Atoi(maxConcurrency); err == nil && val > 0 {
				return val
			}
		}
	}

	// Use real-time metrics if available
	if s.cache != nil {
		// Try to get current running requests and estimate capacity
		if runningReq, err := s.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning); err == nil {
			currentRunning := int(runningReq.GetSimpleValue())
			// Use a multiple of current running as estimate
			if currentRunning > 0 {
				return max(DEFAULT_POD_CONCURRENT_CAPACITY, currentRunning*CAPACITY_ESTIMATION_MULTIPLIER)
			}
		}
	}

	// Default estimate: conservative approach, similar to original scheduler logic
	return DEFAULT_POD_CONCURRENT_CAPACITY
}

// min is a simple utility function.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
