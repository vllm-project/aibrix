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

package types

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
)

// A pooled RoutingContext must not carry one request's early admission into the next: a
// stale ReplicaInflightChecked would make the gateway skip the cap, and a stale admitter
// would run the previous request's closure, with its old limit, on a later PD request.
func TestRoutingContextResetClearsEarlyAdmission(t *testing.T) {
	r := &RoutingContext{}
	r.reset(context.Background(), "pd", "m", "", "r1", "")
	oldAdmitterCalls := 0
	r.SetTargetAdmitter(func(*v1.Pod) error {
		oldAdmitterCalls++
		return ErrTargetReplicaInflightExceeded
	})
	r.ReplicaInflightChecked = true
	r.ReplicaInflightAdmitted = true

	r.reset(context.Background(), "pd", "m", "", "r2", "")

	assert.False(t, r.ReplicaInflightChecked)
	assert.False(t, r.ReplicaInflightAdmitted)
	require.NoError(t, r.AdmitTarget(&v1.Pod{}), "a request with no admitter installed admits its target")
	assert.Zero(t, oldAdmitterCalls, "the previous request's admitter must not run")
}
