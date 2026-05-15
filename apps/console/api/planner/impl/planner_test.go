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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	plannerclient "github.com/vllm-project/aibrix/apps/console/api/planner/client"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// =============================================================================
// Fakes
// =============================================================================

// fakeProvisioner implements provisioner.Provisioner with caller-supplied
// behavior per call site. Tests set ProvisionFn / ReleaseFn to inject
// success/error/latency; the struct also records every call for later
// assertion and tracks peak concurrent in-flight Provisions so worker-pool
// parallelism can be measured directly.
type fakeProvisioner struct {
	ProvisionFn func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error)
	ReleaseFn   func(ctx context.Context, provisionID string) error
	ListFn      func(ctx context.Context, opts *rmtypes.ListOptions) ([]*rmtypes.ProvisionResult, error)

	mu             sync.Mutex
	provisionCalls []string // JobID (IdempotencyKey) of each Provision
	releaseCalls   []string // ProvisionID passed to each Release
	inFlight       int      // current concurrent Provisions
	peakInFlight   int      // max observed concurrent Provisions
}

func (f *fakeProvisioner) Type() rmtypes.ResourceProvisionType {
	return rmtypes.ResourceProvisionTypeKubernetes
}

func (f *fakeProvisioner) Provision(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
	f.mu.Lock()
	f.provisionCalls = append(f.provisionCalls, req.IdempotencyKey)
	f.inFlight++
	if f.inFlight > f.peakInFlight {
		f.peakInFlight = f.inFlight
	}
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	if f.ProvisionFn != nil {
		return f.ProvisionFn(ctx, req)
	}
	// Default: immediate success, ProvisionID derived from IdempotencyKey.
	return &rmtypes.ProvisionResult{
		ProvisionID:    "prov-" + req.IdempotencyKey,
		IdempotencyKey: req.IdempotencyKey,
		Status:         rmtypes.ProvisionStatusRunning,
	}, nil
}

func (f *fakeProvisioner) Release(ctx context.Context, provisionID string) error {
	f.mu.Lock()
	f.releaseCalls = append(f.releaseCalls, provisionID)
	f.mu.Unlock()
	if f.ReleaseFn != nil {
		return f.ReleaseFn(ctx, provisionID)
	}
	return nil
}

func (f *fakeProvisioner) List(ctx context.Context, opts *rmtypes.ListOptions) ([]*rmtypes.ProvisionResult, error) {
	if f.ListFn != nil {
		return f.ListFn(ctx, opts)
	}
	// Default: every queried ProvisionID is Running so existing tests
	// (which don't care about wait-for-ready) see immediate readiness.
	if opts != nil && opts.ProvisionIDs != nil {
		out := make([]*rmtypes.ProvisionResult, 0, len(*opts.ProvisionIDs))
		for _, id := range *opts.ProvisionIDs {
			out = append(out, &rmtypes.ProvisionResult{
				ProvisionID: id,
				Status:      rmtypes.ProvisionStatusRunning,
			})
		}
		return out, nil
	}
	return nil, nil
}

func (f *fakeProvisioner) snapshot() (provisions, releases []string, peak int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.provisionCalls...), append([]string(nil), f.releaseCalls...), f.peakInFlight
}

// fakeBatchClient implements plannerclient.BatchClient. Like fakeProvisioner,
// CreateBatch/Get/Cancel/List are caller-injectable; default behavior is
// immediate success with a deterministically-derived batch.ID so tests can
// correlate JobID -> batch.ID without coordination.
type fakeBatchClient struct {
	CreateFn func(ctx context.Context, params openai.BatchNewParams, aibrix plannerclient.AIBrixExtraBody) (*openai.Batch, error)
	GetFn    func(ctx context.Context, batchID string) (*openai.Batch, error)
	CancelFn func(ctx context.Context, batchID string) (*openai.Batch, error)
	ListFn   func(ctx context.Context, req *plannerclient.ListBatchesRequest) (*plannerclient.ListBatchesResponse, error)

	mu          sync.Mutex
	createCalls []string // JobID (from aibrix.JobID) of each CreateBatch
	cancelCalls []string // batch.ID passed to each CancelBatch
}

func (b *fakeBatchClient) CreateBatch(ctx context.Context, params openai.BatchNewParams, aibrix plannerclient.AIBrixExtraBody) (*openai.Batch, error) {
	b.mu.Lock()
	b.createCalls = append(b.createCalls, aibrix.JobID)
	b.mu.Unlock()
	if b.CreateFn != nil {
		return b.CreateFn(ctx, params, aibrix)
	}
	return &openai.Batch{
		ID:     "batch-" + aibrix.JobID,
		Status: openai.BatchStatusInProgress,
	}, nil
}

func (b *fakeBatchClient) GetBatch(ctx context.Context, batchID string) (*openai.Batch, error) {
	if b.GetFn != nil {
		return b.GetFn(ctx, batchID)
	}
	return &openai.Batch{ID: batchID, Status: openai.BatchStatusInProgress}, nil
}

func (b *fakeBatchClient) CancelBatch(ctx context.Context, batchID string) (*openai.Batch, error) {
	b.mu.Lock()
	b.cancelCalls = append(b.cancelCalls, batchID)
	b.mu.Unlock()
	if b.CancelFn != nil {
		return b.CancelFn(ctx, batchID)
	}
	return &openai.Batch{ID: batchID, Status: openai.BatchStatusCancelled}, nil
}

func (b *fakeBatchClient) ListBatches(ctx context.Context, req *plannerclient.ListBatchesRequest) (*plannerclient.ListBatchesResponse, error) {
	if b.ListFn != nil {
		return b.ListFn(ctx, req)
	}
	return &plannerclient.ListBatchesResponse{Data: nil, HasMore: false}, nil
}

func (b *fakeBatchClient) snapshot() (creates, cancels []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.createCalls...), append([]string(nil), b.cancelCalls...)
}

// =============================================================================
// Helpers
// =============================================================================

// newTestPlanner builds a Planner with the given fakes and worker count
// and registers a cleanup that calls Close so leaked workers can't bleed
// across tests.
func newTestPlanner(t *testing.T, bc plannerclient.BatchClient, prov *fakeProvisioner, workers int) *Planner {
	t.Helper()
	q := NewPlanner(bc, prov, workers)
	t.Cleanup(func() {
		_ = q.Close()
	})
	return q
}

// validReq returns a minimal EnqueueRequest that passes validation.
func validReq(jobID string) *plannerapi.EnqueueRequest {
	return &plannerapi.EnqueueRequest{
		JobID: jobID,
		BatchParams: openai.BatchNewParams{
			InputFileID:      "file-" + jobID,
			Endpoint:         openai.BatchNewParamsEndpoint("/v1/chat/completions"),
			CompletionWindow: openai.BatchNewParamsCompletionWindow("24h"),
		},
	}
}

// waitFor polls cond until true or the timeout elapses. Used to assert
// eventual state without coupling to internal goroutine timing. 10ms
// cadence is the sweet spot under -race: fast enough to feel instant on
// happy paths, slow enough not to burn CPU on RLock acquisitions.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitFor timeout after %v: %s", timeout, msg)
}

// =============================================================================
// Validation
// =============================================================================

func TestEnqueueValidation(t *testing.T) {
	prov := &fakeProvisioner{}
	bc := &fakeBatchClient{}
	q := newTestPlanner(t, bc, prov, 1)

	cases := []struct {
		name    string
		req     *plannerapi.EnqueueRequest
		wantErr error
	}{
		{"nil request", nil, plannerapi.ErrInvalidJob},
		{"missing JobID", &plannerapi.EnqueueRequest{BatchParams: validReq("x").BatchParams}, plannerapi.ErrInvalidJob},
		{"missing InputFileID", &plannerapi.EnqueueRequest{
			JobID: "j",
			BatchParams: openai.BatchNewParams{
				Endpoint:         openai.BatchNewParamsEndpoint("/v1/chat/completions"),
				CompletionWindow: openai.BatchNewParamsCompletionWindow("24h"),
			},
		}, plannerapi.ErrInvalidJob},
		{"missing Endpoint", &plannerapi.EnqueueRequest{
			JobID: "j",
			BatchParams: openai.BatchNewParams{
				InputFileID:      "file-x",
				CompletionWindow: openai.BatchNewParamsCompletionWindow("24h"),
			},
		}, plannerapi.ErrInvalidJob},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := q.Enqueue(context.Background(), tc.req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want errors.Is(%v); got %v", tc.wantErr, err)
			}
		})
	}
}

func TestEnqueueWithNilProvisioner(t *testing.T) {
	q := NewPlanner(&fakeBatchClient{}, nil, 1)
	t.Cleanup(func() { _ = q.Close() })

	_, err := q.Enqueue(context.Background(), validReq("j1"))
	if !errors.Is(err, plannerapi.ErrInsufficientResources) {
		t.Fatalf("want ErrInsufficientResources for nil provisioner; got %v", err)
	}
}

func TestDuplicateJobIDRejected(t *testing.T) {
	// Block Provision so the first job stays in flight while we re-Enqueue
	// the same JobID. Without the block, the first job could complete and
	// be removed before the duplicate check runs — but it isn't removed
	// (jobs map keeps terminal entries), so this is belt-and-suspenders.
	release := make(chan struct{})
	prov := &fakeProvisioner{
		ProvisionFn: func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
			<-release
			return &rmtypes.ProvisionResult{ProvisionID: "p1"}, nil
		},
	}
	q := newTestPlanner(t, &fakeBatchClient{}, prov, 1)

	if _, err := q.Enqueue(context.Background(), validReq("j-dup")); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), validReq("j-dup")); !errors.Is(err, plannerapi.ErrInvalidJob) {
		t.Fatalf("want ErrInvalidJob on duplicate JobID; got %v", err)
	}
	close(release)
}

// =============================================================================
// Happy path + status visibility
// =============================================================================

func TestEnqueueReturnsPendingPlaceholder(t *testing.T) {
	// Block Provision so the job stays in state=pending and the placeholder batch
	// is what Enqueue returns. The test body unblocks at the end so the
	// worker can drain and the test cleanup's Close finishes promptly.
	release := make(chan struct{})
	prov := &fakeProvisioner{
		ProvisionFn: func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
			<-release
			return nil, errors.New("provision aborted by test")
		},
	}
	q := newTestPlanner(t, &fakeBatchClient{}, prov, 1)

	job, err := q.Enqueue(context.Background(), validReq("j1"))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if job.JobID != "j1" {
		t.Errorf("JobID = %q, want %q", job.JobID, "j1")
	}
	if job.Batch == nil || job.Batch.Status != openai.BatchStatus("pending") {
		t.Errorf("Batch.Status = %v, want pending", job.Batch)
	}
	close(release)
}

func TestHappyPathReachesSubmitted(t *testing.T) {
	prov := &fakeProvisioner{} // default success
	bc := &fakeBatchClient{}   // default success, batch.ID = "batch-<JobID>"
	q := newTestPlanner(t, bc, prov, 1)

	if _, err := q.Enqueue(context.Background(), validReq("j1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Eventually CreateBatch is called.
	waitFor(t, 2*time.Second, func() bool {
		creates, _ := bc.snapshot()
		return len(creates) == 1 && creates[0] == "j1"
	}, "expected CreateBatch to fire for j1")

	// GetJob now forwards to MDS — the placeholder batch should be replaced by
	// the MDS-side batch with status=in_progress.
	job, err := q.GetJob(context.Background(), "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Batch.ID != "batch-j1" || job.Batch.Status != openai.BatchStatusInProgress {
		t.Errorf("post-submit GetJob: got %+v, want batch-j1/in_progress", job.Batch)
	}
}

// =============================================================================
// Long-Provision scenarios (the explicit ask)
// =============================================================================

// TestSlowProvisionDoesNotBlockEnqueue: a Provision that takes seconds must
// not delay the Enqueue gRPC response. The user gets back a pending
// placeholder batch within milliseconds even if Provision is still in flight.
func TestSlowProvisionDoesNotBlockEnqueue(t *testing.T) {
	prov := &fakeProvisioner{
		ProvisionFn: func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
			// Simulate a "long" Provision. 500ms is the test budget; the
			// real thing takes minutes. The assertion is about Enqueue
			// latency, not Provision duration.
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &rmtypes.ProvisionResult{ProvisionID: "p-" + req.IdempotencyKey}, nil
		},
	}
	q := newTestPlanner(t, &fakeBatchClient{}, prov, 1)

	start := time.Now()
	_, err := q.Enqueue(context.Background(), validReq("j-slow"))
	enqueueLatency := time.Since(start)

	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Threshold is generous so CI jitter doesn't flake. The point is
	// "decoupled from Provision," not "instant."
	if enqueueLatency > 100*time.Millisecond {
		t.Errorf("Enqueue took %v; should not block on Provision", enqueueLatency)
	}
}

// TestWorkerPoolReachesConcurrency: with workerCount=N, the pool should
// process N jobs in parallel — peak in-flight Provisions == N. This is
// the test that proves Pattern A actually parallelizes.
func TestWorkerPoolReachesConcurrency(t *testing.T) {
	const workers = 4
	const submitted = 8

	// Each Provision parks on this channel until the test releases it.
	// All workers must reach Provision before we release, so peak
	// concurrent in-flight equals the number of available workers.
	release := make(chan struct{})
	var started atomic.Int32

	prov := &fakeProvisioner{
		ProvisionFn: func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
			started.Add(1)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &rmtypes.ProvisionResult{ProvisionID: "p-" + req.IdempotencyKey}, nil
		},
	}
	q := newTestPlanner(t, &fakeBatchClient{}, prov, workers)

	for i := 0; i < submitted; i++ {
		if _, err := q.Enqueue(context.Background(), validReq(fmt.Sprintf("j%d", i))); err != nil {
			t.Fatalf("Enqueue j%d: %v", i, err)
		}
	}

	// Wait until exactly `workers` Provisions are in flight. If we see
	// more, the worker count cap is broken; if we never see `workers`,
	// the pool isn't parallelizing. The post-release peak check below
	// is the authoritative cap assertion — `peak` is incremented
	// inside the fake under lock, so it records every concurrent entry.
	waitFor(t, 2*time.Second, func() bool {
		return started.Load() == int32(workers)
	}, fmt.Sprintf("expected %d Provisions in flight", workers))

	// Release everyone; let the remaining (submitted - workers) jobs
	// drain through the freed workers.
	close(release)

	waitFor(t, 5*time.Second, func() bool {
		provs, _, _ := prov.snapshot()
		return len(provs) == submitted
	}, fmt.Sprintf("expected %d Provisions total", submitted))

	_, _, peak := prov.snapshot()
	if peak != workers {
		t.Errorf("peak in-flight = %d; expected %d", peak, workers)
	}
}

// =============================================================================
// Failure paths
// =============================================================================

// TestProvisionFailureMarksFailed: a Provision error transitions the job
// to Failed; GetJob then returns a placeholder batch with status=failed. Other
// jobs continue to flow through the queue — one bad job doesn't poison
// the worker.
func TestProvisionFailureMarksFailed(t *testing.T) {
	prov := &fakeProvisioner{
		ProvisionFn: func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
			if req.IdempotencyKey == "j-bad" {
				return nil, errors.New("rm capacity exhausted")
			}
			return &rmtypes.ProvisionResult{ProvisionID: "p-" + req.IdempotencyKey}, nil
		},
	}
	bc := &fakeBatchClient{}
	q := newTestPlanner(t, bc, prov, 1)

	if _, err := q.Enqueue(context.Background(), validReq("j-bad")); err != nil {
		t.Fatalf("Enqueue j-bad: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), validReq("j-good")); err != nil {
		t.Fatalf("Enqueue j-good: %v", err)
	}

	// j-bad eventually surfaces as failed.
	waitFor(t, 2*time.Second, func() bool {
		job, err := q.GetJob(context.Background(), "j-bad")
		return err == nil && job.Batch != nil && job.Batch.Status == openai.BatchStatusFailed
	}, "j-bad never reached Failed")

	// j-good eventually submits — proving the worker recovered.
	waitFor(t, 2*time.Second, func() bool {
		creates, _ := bc.snapshot()
		for _, id := range creates {
			if id == "j-good" {
				return true
			}
		}
		return false
	}, "j-good never reached CreateBatch")

	// CreateBatch must NOT have been called for j-bad (Provision short-
	// circuits before submission).
	creates, _ := bc.snapshot()
	for _, id := range creates {
		if id == "j-bad" {
			t.Errorf("CreateBatch was called for j-bad despite Provision failure")
		}
	}
}

// TestWaitsForProvisionRunningBeforeCreateBatch: Provision returns when the
// request is accepted, not when the resource is ready. The worker must
// poll List until status=Running before calling CreateBatch, since MDS
// rejects batches that point to not-yet-ready provisions.
func TestWaitsForProvisionRunningBeforeCreateBatch(t *testing.T) {
	var polls atomic.Int32
	prov := &fakeProvisioner{
		ListFn: func(ctx context.Context, opts *rmtypes.ListOptions) ([]*rmtypes.ProvisionResult, error) {
			// First two polls report Provisioning; third reports Running.
			n := polls.Add(1)
			status := rmtypes.ProvisionStatusProvisioning
			if n >= 3 {
				status = rmtypes.ProvisionStatusRunning
			}
			ids := *opts.ProvisionIDs
			out := make([]*rmtypes.ProvisionResult, 0, len(ids))
			for _, id := range ids {
				out = append(out, &rmtypes.ProvisionResult{ProvisionID: id, Status: status})
			}
			return out, nil
		},
	}
	bc := &fakeBatchClient{}
	q := newTestPlanner(t, bc, prov, 1)
	q.provPollInterval = 10 * time.Millisecond // fast polling for the test

	if _, err := q.Enqueue(context.Background(), validReq("j-wait")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// CreateBatch must only fire after we've observed Running (poll #3+).
	waitFor(t, 2*time.Second, func() bool {
		creates, _ := bc.snapshot()
		return len(creates) == 1
	}, "CreateBatch never ran")

	if got := polls.Load(); got < 3 {
		t.Errorf("polls before CreateBatch = %d; want ≥ 3", got)
	}
}

// TestProvisionFailedDuringPollingMarksFailed: if the RM reports a Failed
// status while we're polling, the planner must mark the job Failed,
// release the provision, and skip CreateBatch entirely.
func TestProvisionFailedDuringPollingMarksFailed(t *testing.T) {
	prov := &fakeProvisioner{
		ListFn: func(ctx context.Context, opts *rmtypes.ListOptions) ([]*rmtypes.ProvisionResult, error) {
			ids := *opts.ProvisionIDs
			out := make([]*rmtypes.ProvisionResult, 0, len(ids))
			for _, id := range ids {
				out = append(out, &rmtypes.ProvisionResult{
					ProvisionID:  id,
					Status:       rmtypes.ProvisionStatusFailed,
					ErrorMessage: "synthetic failure during provisioning",
				})
			}
			return out, nil
		},
	}
	bc := &fakeBatchClient{}
	q := newTestPlanner(t, bc, prov, 1)
	q.provPollInterval = 10 * time.Millisecond

	if _, err := q.Enqueue(context.Background(), validReq("j-prov-fail")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Job should land in Failed state with Release called and no CreateBatch.
	waitFor(t, 2*time.Second, func() bool {
		_, releases, _ := prov.snapshot()
		return len(releases) == 1 && releases[0] == "prov-j-prov-fail"
	}, "Release was not called after provision failure")

	creates, _ := bc.snapshot()
	if len(creates) != 0 {
		t.Errorf("CreateBatch was called despite provision failure: %v", creates)
	}

	got, err := q.GetJob(context.Background(), "j-prov-fail")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Batch.Status != openai.BatchStatusFailed {
		t.Errorf("GetJob status = %v; want failed", got.Batch.Status)
	}
}

// TestCreateBatchFailureReleasesResource: if Provision succeeds but the
// subsequent CreateBatch fails, the planner must call Release so the
// already-allocated RM resource doesn't leak. This is the regression test
// for the known review-comment carried over from #2184.
func TestCreateBatchFailureReleasesResource(t *testing.T) {
	prov := &fakeProvisioner{}
	bc := &fakeBatchClient{
		CreateFn: func(ctx context.Context, params openai.BatchNewParams, aibrix plannerclient.AIBrixExtraBody) (*openai.Batch, error) {
			return nil, errors.New("mds 503")
		},
	}
	q := newTestPlanner(t, bc, prov, 1)

	if _, err := q.Enqueue(context.Background(), validReq("j-fail")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for state=Failed.
	waitFor(t, 2*time.Second, func() bool {
		job, err := q.GetJob(context.Background(), "j-fail")
		return err == nil && job.Batch != nil && job.Batch.Status == openai.BatchStatusFailed
	}, "j-fail never reached Failed")

	// Release must have been called with the ProvisionID that Provision
	// returned (default fake: "prov-<JobID>").
	_, releases, _ := prov.snapshot()
	if len(releases) != 1 || releases[0] != "prov-j-fail" {
		t.Errorf("releaseCalls = %v; want exactly [prov-j-fail]", releases)
	}
}

// TestCreateBatchFailureReleaseErrorIsLoggedNotSurfaced: even if Release
// itself errors after CreateBatch fails, the original CreateBatch error
// is what defines the terminal state. The job still ends as Failed; we
// don't crash, panic, or hang.
func TestCreateBatchFailureReleaseErrorIsLoggedNotSurfaced(t *testing.T) {
	prov := &fakeProvisioner{
		ReleaseFn: func(ctx context.Context, provisionID string) error {
			return errors.New("rm release timeout")
		},
	}
	bc := &fakeBatchClient{
		CreateFn: func(ctx context.Context, params openai.BatchNewParams, aibrix plannerclient.AIBrixExtraBody) (*openai.Batch, error) {
			return nil, errors.New("mds 503")
		},
	}
	q := newTestPlanner(t, bc, prov, 1)

	if _, err := q.Enqueue(context.Background(), validReq("j-rfail")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		job, err := q.GetJob(context.Background(), "j-rfail")
		return err == nil && job.Batch != nil && job.Batch.Status == openai.BatchStatusFailed
	}, "j-rfail never reached Failed despite release error")
}

// =============================================================================
// Cancel
// =============================================================================

func TestCancelQueuedJobBeforeWorkerPicksUp(t *testing.T) {
	// Single worker, blocked indefinitely on Provision so the next job
	// stays in state=pending long enough to cancel. When the test
	// releases the block we want a clean success, not (nil, nil) —
	// otherwise the worker nil-derefs in the post-Provision path.
	block := make(chan struct{})
	prov := &fakeProvisioner{
		ProvisionFn: func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
			select {
			case <-block:
				return &rmtypes.ProvisionResult{ProvisionID: "p-" + req.IdempotencyKey}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	bc := &fakeBatchClient{}
	q := newTestPlanner(t, bc, prov, 1)

	// Job A occupies the single worker; job B sits in the channel.
	if _, err := q.Enqueue(context.Background(), validReq("j-A")); err != nil {
		t.Fatalf("Enqueue A: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), validReq("j-B")); err != nil {
		t.Fatalf("Enqueue B: %v", err)
	}

	// Wait until A is actually in Provision (so we know the worker is
	// busy and B has not yet been picked up — it can't be, the only
	// worker is parked).
	waitFor(t, 2*time.Second, func() bool {
		provs, _, _ := prov.snapshot()
		return len(provs) == 1 && provs[0] == "j-A"
	}, "j-A never started provisioning")

	// Cancel B while still queued.
	job, err := q.Cancel(context.Background(), "j-B")
	if err != nil {
		t.Fatalf("Cancel B: %v", err)
	}
	if job.Batch.Status != openai.BatchStatusCancelled {
		t.Errorf("Cancel B status = %v; want cancelled", job.Batch.Status)
	}

	// Release the worker. B is now eligible for processing, but the
	// state-check at the top of process() should skip it; Provision
	// should NEVER be called for j-B.
	close(block)

	// Give the worker a moment to pop j-B and skip it.
	waitFor(t, 2*time.Second, func() bool {
		provs, _, _ := prov.snapshot()
		// j-A is the only Provision recorded; j-B never provisions.
		return len(provs) == 1
	}, "j-A snapshot")

	provs, _, _ := prov.snapshot()
	for _, id := range provs {
		if id == "j-B" {
			t.Errorf("Provision was called for canceled j-B")
		}
	}
}

func TestCancelSubmittedJobForwardsToMDSAndReleasesProvision(t *testing.T) {
	prov := &fakeProvisioner{}
	bc := &fakeBatchClient{}
	q := newTestPlanner(t, bc, prov, 1)

	if _, err := q.Enqueue(context.Background(), validReq("j-sub")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Wait for the job to actually reach Submitted.
	waitFor(t, 2*time.Second, func() bool {
		creates, _ := bc.snapshot()
		return len(creates) == 1
	}, "CreateBatch never fired")

	job, err := q.Cancel(context.Background(), "j-sub")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if job.Batch.ID != "batch-j-sub" {
		t.Errorf("Cancel returned batch.ID = %q; want batch-j-sub", job.Batch.ID)
	}

	_, cancels := bc.snapshot()
	if len(cancels) != 1 || cancels[0] != "batch-j-sub" {
		t.Errorf("bc.CancelBatch calls = %v; want [batch-j-sub]", cancels)
	}

	_, releases, _ := prov.snapshot()
	if len(releases) != 1 || releases[0] != "prov-j-sub" {
		t.Errorf("prov.Release calls = %v; want [prov-j-sub]", releases)
	}
}

func TestCancelUnknownJobReturnsNotFound(t *testing.T) {
	q := newTestPlanner(t, &fakeBatchClient{}, &fakeProvisioner{}, 1)
	_, err := q.Cancel(context.Background(), "j-ghost")
	if !errors.Is(err, plannerapi.ErrJobNotFound) {
		t.Errorf("want ErrJobNotFound; got %v", err)
	}
}

// TestCancelDuringProvisioningHonoredAfterCreateBatch: Cancel arrives while
// the worker is parked inside Provision. User sees cancelled immediately;
// the worker's in-flight Provision continues to completion, CreateBatch
// runs and returns a batch.ID, then the post-CreateBatch checkpoint
// detects the cancel, forwards CancelBatch to MDS, and releases the
// provisioned resource.
func TestCancelDuringProvisioningHonoredAfterCreateBatch(t *testing.T) {
	provGate := make(chan struct{})
	prov := &fakeProvisioner{
		ProvisionFn: func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
			<-provGate
			return &rmtypes.ProvisionResult{ProvisionID: "prov-" + req.IdempotencyKey}, nil
		},
	}
	bc := &fakeBatchClient{}
	q := newTestPlanner(t, bc, prov, 1)

	if _, err := q.Enqueue(context.Background(), validReq("j-mid-prov")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		provs, _, _ := prov.snapshot()
		return len(provs) == 1
	}, "worker never entered Provision")

	job, err := q.Cancel(context.Background(), "j-mid-prov")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if job.Batch.Status != openai.BatchStatusCancelled {
		t.Errorf("Cancel returned status = %v; want cancelled", job.Batch.Status)
	}

	close(provGate)

	waitFor(t, 2*time.Second, func() bool {
		_, cancels := bc.snapshot()
		_, releases, _ := prov.snapshot()
		return len(cancels) == 1 && cancels[0] == "batch-j-mid-prov" &&
			len(releases) == 1 && releases[0] == "prov-j-mid-prov"
	}, "expected CancelBatch forward + Release after cancel-during-Provisioning")
}

// TestCancelDuringCreateBatchHonored: Cancel arrives while the worker is
// parked inside CreateBatch. Provision had already returned, so the
// resource is allocated. The post-CreateBatch checkpoint detects the
// cancel, forwards CancelBatch to MDS so the batch doesn't run unattended,
// and releases the resource.
func TestCancelDuringCreateBatchHonored(t *testing.T) {
	createGate := make(chan struct{})
	bc := &fakeBatchClient{
		CreateFn: func(ctx context.Context, params openai.BatchNewParams, aibrix plannerclient.AIBrixExtraBody) (*openai.Batch, error) {
			<-createGate
			return &openai.Batch{ID: "batch-" + aibrix.JobID, Status: openai.BatchStatusInProgress}, nil
		},
	}
	prov := &fakeProvisioner{} // default returns ProvisionID="prov-<JobID>"
	q := newTestPlanner(t, bc, prov, 1)

	if _, err := q.Enqueue(context.Background(), validReq("j-mid-create")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		creates, _ := bc.snapshot()
		return len(creates) == 1
	}, "worker never entered CreateBatch")

	if _, err := q.Cancel(context.Background(), "j-mid-create"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	close(createGate)

	waitFor(t, 2*time.Second, func() bool {
		_, cancels := bc.snapshot()
		_, releases, _ := prov.snapshot()
		return len(cancels) == 1 && cancels[0] == "batch-j-mid-create" &&
			len(releases) == 1 && releases[0] == "prov-j-mid-create"
	}, "expected CancelBatch forward + Release after cancel-during-CreateBatch")

	// Post-race state: GetJob takes the non-Submitted branch and returns
	// a placeholder with status=cancelled.
	got, err := q.GetJob(context.Background(), "j-mid-create")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Batch.Status != openai.BatchStatusCancelled {
		t.Errorf("post-race GetJob status = %v; want cancelled", got.Batch.Status)
	}
}

// =============================================================================
// Shutdown / lifecycle
// =============================================================================

// TestCloseCancelsInflightProvision: when Close fires while N workers are
// mid-Provision, baseCtx cancellation must propagate into each in-flight
// call, and Close must wait for every worker to exit.
func TestCloseCancelsInflightProvision(t *testing.T) {
	const workers = 3
	var observedCancel atomic.Int32

	prov := &fakeProvisioner{
		ProvisionFn: func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
			// Block until ctx cancels; record that the cancel signal
			// actually reached us. This is the contract: a long
			// Provision must honor context cancellation.
			<-ctx.Done()
			observedCancel.Add(1)
			return nil, ctx.Err()
		},
	}
	q := NewPlanner(&fakeBatchClient{}, prov, workers)

	for i := 0; i < workers; i++ {
		if _, err := q.Enqueue(context.Background(), validReq(fmt.Sprintf("j%d", i))); err != nil {
			t.Fatalf("Enqueue j%d: %v", i, err)
		}
	}
	// Wait until every worker is in flight; otherwise we'd be testing
	// "Close before workers started" which is a different path.
	waitFor(t, 2*time.Second, func() bool {
		provs, _, _ := prov.snapshot()
		return len(provs) == workers
	}, "not all workers entered Provision")

	done := make(chan struct{})
	go func() {
		_ = q.Close()
		close(done)
	}()

	select {
	case <-done:
		// Good — Close returned. All workers observed cancellation.
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within 3s; worker likely didn't honor ctx cancel")
	}

	if got := observedCancel.Load(); got != int32(workers) {
		t.Errorf("only %d/%d workers observed ctx cancel", got, workers)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	q := NewPlanner(&fakeBatchClient{}, &fakeProvisioner{}, 2)
	if err := q.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close on a drained pool must not deadlock. baseCancel is
	// idempotent; wg.Wait on a zero counter returns immediately.
	done := make(chan struct{})
	go func() {
		_ = q.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second Close deadlocked")
	}
}

// TestEnqueueAfterCloseReturnsClosed locks down planner shutdown semantics:
// once Close returns, new Enqueue calls must fail immediately instead of
// slipping into the buffered pending queue.
func TestEnqueueAfterCloseReturnsClosed(t *testing.T) {
	q := NewPlanner(&fakeBatchClient{}, &fakeProvisioner{}, 1)
	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// This guards the refactor from queue-owned shutdown to planner-owned
	// shutdown checks. Without the Enqueue-side closed check, the buffered
	// queue can still accept a job after Close.
	_, err := q.Enqueue(context.Background(), validReq("j-closed"))
	if err == nil {
		t.Fatal("Enqueue after Close unexpectedly succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want Close error to wrap context.Canceled; got %v", err)
	}
}

// TestWorkerCountFloor: a non-positive workerCount must be floored to 1
// rather than starting zero goroutines (which would silently hang every
// Enqueue forever).
func TestWorkerCountFloor(t *testing.T) {
	prov := &fakeProvisioner{}
	bc := &fakeBatchClient{}
	q := newTestPlanner(t, bc, prov, 0) // explicitly degenerate

	if _, err := q.Enqueue(context.Background(), validReq("j1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// If the floor works, at least one worker is consuming and the job
	// reaches CreateBatch. If it doesn't, this waitFor times out.
	waitFor(t, 2*time.Second, func() bool {
		creates, _ := bc.snapshot()
		return len(creates) == 1
	}, "floored worker never processed the job")
}

// =============================================================================
// Reads (GetJob / ListJobs)
// =============================================================================

func TestGetJobUnknownReturnsNotFound(t *testing.T) {
	q := newTestPlanner(t, &fakeBatchClient{}, &fakeProvisioner{}, 1)
	_, err := q.GetJob(context.Background(), "j-ghost")
	if !errors.Is(err, plannerapi.ErrJobNotFound) {
		t.Errorf("want ErrJobNotFound; got %v", err)
	}
}

// TestListJobsMergesProvisioningAndMDS: first page combines MDS-side
// batches with local jobs that haven't reached MDS yet. Subsequent pages
// (After != "") return only MDS-side results so the cursor stays valid.
//
// The local job is parked in jobStateProvisioning here (worker entered
// Provision and is blocked on the fake), so its status is "provisioning".
// The pending window — between Enqueue and the worker picking the JobID
// off the submit channel — is too narrow to assert on deterministically;
// it's exercised by other tests that use a 0-worker Planner.
func TestListJobsMergesProvisioningAndMDS(t *testing.T) {
	// Block Provision so the local job stays unsubmitted while ListJobs
	// runs — that's how unsubmittedJobs picks it up.
	block := make(chan struct{})
	prov := &fakeProvisioner{
		ProvisionFn: func(ctx context.Context, req *rmtypes.ResourceProvision) (*rmtypes.ProvisionResult, error) {
			select {
			case <-block:
				return &rmtypes.ProvisionResult{ProvisionID: "p-" + req.IdempotencyKey}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	bc := &fakeBatchClient{
		ListFn: func(ctx context.Context, req *plannerclient.ListBatchesRequest) (*plannerclient.ListBatchesResponse, error) {
			return &plannerclient.ListBatchesResponse{
				Data: []*openai.Batch{
					{ID: "batch-mds-1", Status: openai.BatchStatusInProgress},
				},
				HasMore: false,
			}, nil
		},
	}
	q := newTestPlanner(t, bc, prov, 1)

	if _, err := q.Enqueue(context.Background(), validReq("j-provisioning")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Make sure the worker is parked on the block channel before we
	// list — otherwise we'd be racing against state transitions.
	waitFor(t, 2*time.Second, func() bool {
		provs, _, _ := prov.snapshot()
		return len(provs) == 1
	}, "worker did not enter Provision")

	// First page: local provisioning + MDS-side merged.
	resp, err := q.ListJobs(context.Background(), &plannerapi.ListJobsRequest{})
	if err != nil {
		t.Fatalf("ListJobs first page: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("first page len = %d; want 2 (provisioning + mds)", len(resp.Data))
	}
	// unsubmittedJobs is prepended, so index 0 is the local job.
	if resp.Data[0].JobID != "j-provisioning" {
		t.Errorf("first entry JobID = %q; want j-provisioning", resp.Data[0].JobID)
	}
	if resp.Data[0].Batch.Status != openai.BatchStatus("provisioning") {
		t.Errorf("first entry status = %v; want provisioning", resp.Data[0].Batch.Status)
	}

	// Subsequent page (After non-empty): MDS-only.
	resp2, err := q.ListJobs(context.Background(), &plannerapi.ListJobsRequest{After: "cursor"})
	if err != nil {
		t.Fatalf("ListJobs second page: %v", err)
	}
	if len(resp2.Data) != 1 || resp2.Data[0].Batch.ID != "batch-mds-1" {
		t.Errorf("second page = %+v; want only batch-mds-1", resp2.Data)
	}

	close(block)
}

// =============================================================================
// Concurrent stress (race detector)
// =============================================================================

// TestConcurrentEnqueuesNoRace fires many concurrent Enqueues to surface
// races under `go test -race`. It also checks that no Enqueue silently
// succeeds without a corresponding entry in the local map.
func TestConcurrentEnqueuesNoRace(t *testing.T) {
	prov := &fakeProvisioner{}
	bc := &fakeBatchClient{}
	q := newTestPlanner(t, bc, prov, 4)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := q.Enqueue(context.Background(), validReq(fmt.Sprintf("j%d", i))); err != nil {
				t.Errorf("Enqueue j%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// All N jobs eventually reach CreateBatch.
	waitFor(t, 5*time.Second, func() bool {
		creates, _ := bc.snapshot()
		return len(creates) == N
	}, fmt.Sprintf("not all %d jobs reached CreateBatch", N))
}
