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

package planner

import "errors"

// Sentinel errors. Planner implementations should wrap with %w so callers can
// use errors.Is without parsing transport- or storage-specific error strings.
var (
	// ErrInvalidJob indicates the submitted PlannerJob failed validation
	// (missing required fields, etc.).
	ErrInvalidJob = errors.New("planner: invalid job")

	// ErrJobNotFound indicates the requested JobID has no matching task in
	// the planner store.
	ErrJobNotFound = errors.New("planner: job not found")

	// ErrStoreFull indicates a bounded store rejected an enqueue request
	// because pending capacity is exhausted.
	ErrStoreFull = errors.New("planner: store full")

	// ErrStoreUnavailable indicates the store backend could not accept or
	// serve requests (network/storage outage, dependency timeout, etc.).
	ErrStoreUnavailable = errors.New("planner: store unavailable")

	// ErrDuplicateEnqueue indicates the requested job/idempotency key is
	// already present in the store.
	ErrDuplicateEnqueue = errors.New("planner: duplicate enqueue")

	// ErrMDSSubmitFailed indicates submitting the OpenAI batch to MDS
	// failed. The Worker wraps upstream BatchClient.CreateBatch errors
	// with this sentinel when the failure occurred after planning but
	// before a batch ID was durably recorded, so callers can route on
	// errors.Is without parsing transport-specific error strings.
	ErrMDSSubmitFailed = errors.New("planner: mds submit failed")

	// ErrInsufficientResources indicates the RM could not satisfy a
	// capacity request right now. The worker should typically Nack the
	// task with backoff and retry later. The RM's typed error (from
	// the adjacent RM package) is wrapped with this sentinel at the
	// worker-store boundary so the planner stays decoupled from the
	// concrete RM error vocabulary.
	ErrInsufficientResources = errors.New("planner: insufficient resources")

	// ErrTaskAlreadyTerminal indicates EnqueueContinuation targeted a
	// task that has already reached a terminal state (terminal_failure,
	// superseded, or any post-submit MDS-driven terminal — for example
	// MDS finished the batch before the reservation-expiry sweeper
	// saw it). Callers may treat this as a no-op success - the task is
	// settled - but the sentinel is exposed so the reservation-expiry
	// sweeper can distinguish "raced with MDS-side completion" from
	// real errors and skip without alerting.
	ErrTaskAlreadyTerminal = errors.New("planner: task already terminal")
)
