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

	// ErrLeaseLost indicates an Ack/Nack/Fail/RenewLease call carried a lease
	// the store no longer recognizes (typically because the lease expired and
	// another worker re-leased the task). The caller should drop the in-flight
	// work.
	ErrLeaseLost = errors.New("planner: lease lost")

	// ErrMDSSubmitFailed indicates submitting the OpenAI batch to MDS
	// failed. TaskExecutor implementations should wrap upstream submit errors
	// with this sentinel when the failure occurred after planning but before a
	// batch ID was durably recorded.
	ErrMDSSubmitFailed = errors.New("planner: mds submit failed")

	// ErrInsufficientResources indicates the ResourceManager could not
	// satisfy a Reserve request right now. The worker should typically Nack
	// the task with backoff and retry later.
	ErrInsufficientResources = errors.New("planner: insufficient resources")

	// ErrResourceManagerUnavailable indicates the RM backend could not accept
	// or serve requests (network/storage outage, dependency timeout, etc.).
	ErrResourceManagerUnavailable = errors.New("planner: resource manager unavailable")

	// ErrReservationNotFound indicates a Release call referenced a reservation
	// the RM no longer recognizes (already released or expired). Callers may
	// treat this as a no-op success.
	ErrReservationNotFound = errors.New("planner: reservation not found")

	// ErrReservationExpired indicates an attempt to use or extend a reservation
	// that the RM already reclaimed. The worker should re-reserve before
	// continuing.
	ErrReservationExpired = errors.New("planner: reservation expired")

	// ErrTaskAlreadyTerminal indicates a non-lease state-transition call
	// (CancelTask, EnqueueContinuation) targeted a task that has already
	// reached a terminal state. Callers may treat this as a no-op
	// success - the task is settled - but the sentinel is exposed so
	// the reservation-expiry sweeper can distinguish "raced with
	// MDS-side completion" from real errors and skip without alerting.
	ErrTaskAlreadyTerminal = errors.New("planner: task already terminal")
)
