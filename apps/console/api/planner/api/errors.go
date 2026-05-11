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
var (
	// ErrInvalidJob indicates the EnqueueRequest failed validation
	// (missing required fields, etc.).
	ErrInvalidJob = errors.New("planner: invalid job")

	// ErrInsufficientResources indicates the RM could not satisfy a
	// capacity request right now. The RM's typed error (from the adjacent
	// RM package) is wrapped with this sentinel so the planner stays
	// decoupled from the concrete RM error vocabulary.
	ErrInsufficientResources = errors.New("planner: insufficient resources")

	// ErrJobNotFound indicates the JobID passed to GetJob/Cancel is not
	// known to the planner. With Passthrough's in-memory map this fires
	// for jobs created before the process started; the queued planner's
	// durable index will narrow this to "truly unknown JobID" cases.
	ErrJobNotFound = errors.New("planner: job not found")
)
