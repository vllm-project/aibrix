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

package routingalgorithms

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"

	v1 "k8s.io/api/core/v1"
)

func TestSessionAffinityRouter(t *testing.T) {
	tests := []struct {
		name                string
		reqHeaders          map[string]string
		readyPods           []*v1.Pod
		expectErr           bool
		expectPossibleAddrs []string // all valid target addresses (IP:port) that may be selected
	}{
		{
			name: "valid session ID matches ready pod",
			reqHeaders: map[string]string{
				sessionIDHeader: base64.StdEncoding.EncodeToString([]byte("10.0.0.2:8000")),
			},
			readyPods: []*v1.Pod{
				newPod("pod1", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod3", "10.0.0.3", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			expectErr:           false,
			expectPossibleAddrs: []string{"10.0.0.2:8000"},
		},
		{
			name:       "no session ID → fallback to any ready pod",
			reqHeaders: nil,
			readyPods: []*v1.Pod{
				newPod("pod1", "10.0.0.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("pod2", "10.0.0.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			expectErr:           false,
			expectPossibleAddrs: []string{"10.0.0.1:8000", "10.0.0.2:8000"},
		},
		{
			name: "invalid base64 session ID → fallback",
			reqHeaders: map[string]string{
				sessionIDHeader: "%%%INVALID_BASE64%%%",
			},
			readyPods: []*v1.Pod{
				newPod("a", "192.168.1.10", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("b", "192.168.1.11", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			expectErr:           false,
			expectPossibleAddrs: []string{"192.168.1.10:8000", "192.168.1.11:8000"},
		},
		{
			name: "session ID points to non-existent address → fallback",
			reqHeaders: map[string]string{
				sessionIDHeader: base64.StdEncoding.EncodeToString([]byte("10.99.99.99:8000")), // 不存在的 IP
			},
			readyPods: []*v1.Pod{
				newPod("x", "10.1.1.1", true, map[string]string{"model.aibrix.ai/port": "8000"}),
				newPod("y", "10.1.1.2", true, map[string]string{"model.aibrix.ai/port": "8000"}),
			},
			expectErr:           false,
			expectPossibleAddrs: []string{"10.1.1.1:8000", "10.1.1.2:8000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := &sessionAffinityRouter{}

			ctx := types.NewRoutingContext(context.Background(), "test", "model1", "", "", "")
			ctx.ReqHeaders = tt.reqHeaders

			podList := newMockPodList(tt.readyPods, nil)

			addr, err := router.Route(ctx, podList)

			if tt.expectErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.NotNil(t, ctx.RespHeaders, "RespHeaders should not be nil")
			assert.Contains(t, ctx.RespHeaders, sessionIDHeader, "Response must include session ID header")

			// verify the returned address is one of the expected ready pod addresses
			assert.Contains(t, tt.expectPossibleAddrs, addr, "selected address must be one of the ready pods' IP:port")

			// verify that the session ID in the response decodes to the same address
			sessionB64 := ctx.RespHeaders[sessionIDHeader]
			sessionBytes, decodeErr := base64.StdEncoding.DecodeString(sessionB64)
			assert.NoError(t, decodeErr, "session ID must be valid base64")
			actualSessionAddr := string(sessionBytes)

			assert.Equal(t, addr, actualSessionAddr, "session ID must encode the same address as returned by Route()")
		})
	}
}
