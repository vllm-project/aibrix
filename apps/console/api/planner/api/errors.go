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

package plannerapi

import "errors"

// Sentinel errors returned by the Planner interface. Implementations
// should wrap with %w so callers can use errors.Is without parsing
// transport- or storage-specific error strings.
//
// Planner-internal coordination errors (e.g. ErrTaskAlreadyTerminal,
// ErrMDSSubmitFailed) do not appear here; they live alongside the
// internal interfaces that surface them and are not part of the
// Console-facing contract.
var (
	// ErrInvalidJob indicates the EnqueueRequest failed validation
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

	// ErrInsufficientResources indicates the RM could not satisfy a
	// capacity request right now. The worker should typically Nack the
	// task with backoff and retry later. The RM's typed error (from
	// the adjacent RM package) is wrapped with this sentinel at the
	// worker-store boundary so the planner stays decoupled from the
	// concrete RM error vocabulary.
	ErrInsufficientResources = errors.New("planner: insufficient resources")
)
