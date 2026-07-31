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

// Package plannerapi defines the Console-facing surface of the AIBrix
// planner: the Planner interface, the request/response shapes
// Enqueue / GetJob / ListJobs / GetQueueStats / GetProvisionResourceStats
// carry, and the typed errors Console handlers route on.
//
// Anything Console (or any other consumer outside the planner module)
// imports lives here. The planner -> MDS adapter is rooted at the
// sibling package .../planner/client; the synchronous reference
// implementation lives at .../planner/impl. Worker / store / scheduler
// packages will join when the async Scheduler lands.
package plannerapi
