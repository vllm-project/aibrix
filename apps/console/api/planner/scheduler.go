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

import "context"

// SchedulerFunc is the convention for plugging a scheduling policy
// into the Worker. Given a TaskStore and a ScheduleRequest, it returns
// the TaskIDs to claim in preferred order. The Worker hands the result
// to TaskStore.ClaimByID for atomic acquisition.
//
// The Worker accepts a SchedulerFunc at construction time; whichever
// function is injected determines the policy. Custom policies are just
// new SchedulerFunc values - no new interface, no new type, no new
// TaskStore methods. Wiring changes amount to one assignment in main().
//
// Convention: a nil SchedulerFunc means "use TaskStore.Claim directly",
// so callers that don't care about ranking can leave it unset and the
// Worker falls through to the store-baked FCFS path.
//
// Reference implementations (FCFS, priority, fair-share, ...) are
// intentionally not shipped here. The type is defined now so the team
// implementing concrete policies and any future RM-aware policies can
// develop in parallel against a stable contract. Concrete SchedulerFunc
// values land in follow-up PRs alongside their consumers.
//
// SchedulerFunc is the function-shaped equivalent of a Scheduler
// interface; if a real plug-in taxonomy is needed later (multiple
// policies in the same binary, exposed by name), the function type
// wraps cleanly inside a one-method interface (see http.HandlerFunc /
// http.Handler for the same pattern in the standard library). Keep it
// as a function type until that need is concrete.
type SchedulerFunc func(ctx context.Context, store TaskStore, req *ScheduleRequest) ([]string, error)
