/*
Copyright 2025 The Aibrix Team.

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

package deploymentstatus

// Canonical deployment lifecycle states exposed by the Console backend.
//
// State transitions:
//
//	+----------+
//	|  Create  |
//	+----------+
//	     |
//	     v
//	+-----------+   observe partial progress   +---------+
//	| Deploying | ---------------------------> | Scaling |
//	+-----------+                              +---------+
//	     ^                                          |
//	     |                                          | enough ready replicas
//	     | reconcile still pending                  v
//	     +------------------------------------- +-------+
//	                                           | Ready |
//	                                           +-------+
//
//	Exceptional transitions:
//
//	+-----------+     progress deadline / missing resource     +--------+
//	| Deploying | -------------------------------------------> | Failed |
//	+-----------+                                             +--------+
//
//	+---------+       progress deadline / missing resource     +--------+
//	| Scaling | ---------------------------------------------> | Failed |
//	+---------+                                               +--------+
//
//	+-------+         service missing                          +----------+
//	| Ready | -----------------------------------------------> | Degraded |
//	+-------+                                                 +----------+
//
//	+-----------+     provider resource gone                   +---------+
//	| Any state | -------------------------------------------> | Deleted |
//	+-----------+                                             +---------+
const (
	StatusReady     = "Ready"
	StatusDeploying = "Deploying"
	StatusScaling   = "Scaling"
	StatusFailed    = "Failed"
	StatusDegraded  = "Degraded"
	StatusDeleted   = "Deleted"
)
