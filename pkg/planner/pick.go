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

// PickFunc is the convention for plugging a scheduling policy into a
// Worker. Given a TaskStore and a PickRequest, it returns the TaskIDs
// to lease in preferred order. The Worker hands the result to
// TaskStore.LeaseByID for atomic acquisition.
//
// Workers accept a PickFunc at construction time; whichever function
// is injected determines the policy. Custom policies are just new
// PickFunc values - no new interface, no new type, no new TaskStore
// methods. Wiring changes amount to one assignment in main().
//
// Convention: a nil PickFunc means "use TaskStore.Lease directly", so
// callers that don't care about ranking can leave it unset and the
// Worker falls through to the store-baked FCFS path.
//
// Reference implementations (FCFS, priority, fair-share, ...) are
// intentionally not shipped here. The type is defined now so the team
// implementing the Worker, the team implementing concrete policies,
// and any future ResourceManager-aware policies can develop in
// parallel against a stable contract. Concrete PickFunc values land
// in follow-up PRs alongside their consumers.
//
// PickFunc is the lightweight stand-in for a TaskScheduler interface;
// if a real plug-in taxonomy is needed later (multiple policies in the
// same binary, exposed by name), the function type wraps cleanly inside
// a one-method interface. Keep it as a function type until that need
// is concrete.
type PickFunc func(ctx context.Context, store TaskStore, req *PickRequest) ([]string, error)
