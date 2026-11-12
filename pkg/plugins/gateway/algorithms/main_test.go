/*
Copyright 2024 The Aibrix Team.

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

package routingalgorithms

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Test caches stamp every Ready pod routable at the moment it is added, which would put every
	// load-balance test inside the capacity warm-up and replace its capacities with 1.0. The
	// tests that cover the warm-up turn it back on (see withCapacityWarmup).
	loadBalanceCapacityWarmup = 0
	os.Exit(m.Run())
}
