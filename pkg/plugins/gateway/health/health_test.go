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

package health

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/health/grpc_health_v1"
)

func TestSimpleHealthServer_Check(t *testing.T) {
	tests := []struct {
		name                  string
		leaderElectionEnabled bool
		isLeader              bool
		service               string
		expectStatus          grpc_health_v1.HealthCheckResponse_ServingStatus
	}{
		// Case 1: Leader election disabled → always SERVING
		{"leader-election-disabled", false, false, "gateway", grpc_health_v1.HealthCheckResponse_SERVING},
		{"leader-election-disabled-liveness", false, true, "liveness", grpc_health_v1.HealthCheckResponse_SERVING},

		// Case 2: Leader election enabled + is leader
		{"leader-enabled-is-leader-readiness", true, true, "readiness", grpc_health_v1.HealthCheckResponse_SERVING},
		{"leader-enabled-is-leader-liveness", true, true, "liveness", grpc_health_v1.HealthCheckResponse_SERVING},
		{"leader-enabled-is-leader-empty", true, true, "", grpc_health_v1.HealthCheckResponse_SERVING},
		{"leader-enabled-is-leader-gateway", true, true, "gateway", grpc_health_v1.HealthCheckResponse_SERVING},

		// Case 3: Leader election enabled + not leader
		{"leader-enabled-not-leader-readiness", true, false, "readiness", grpc_health_v1.HealthCheckResponse_NOT_SERVING},
		{"leader-enabled-not-leader-empty", true, false, "", grpc_health_v1.HealthCheckResponse_NOT_SERVING},
		{"leader-enabled-not-leader-gateway", true, false, "gateway", grpc_health_v1.HealthCheckResponse_NOT_SERVING},
		{"leader-enabled-not-leader-liveness", true, false, "liveness", grpc_health_v1.HealthCheckResponse_SERVING}, // liveness always serves
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var isLeader atomic.Bool
			isLeader.Store(tt.isLeader)

			server := NewHealthServer(&isLeader, tt.leaderElectionEnabled)
			resp, err := server.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: tt.service})
			require.NoError(t, err)
			assert.Equal(t, tt.expectStatus, resp.Status)
		})
	}
}
