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

package external

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

var (
	benchmarkExternalPayload       []byte
	benchmarkExternalAddress       string
	benchmarkExternalSnapshotCount int
)

func benchmarkExternalPods(count int) types.PodList {
	pods := make([]*v1.Pod, 0, count)
	for i := 0; i < count; i++ {
		pods = append(pods, externalTestPod(
			"default",
			fmt.Sprintf("candidate-%03d", i),
			fmt.Sprintf("10.0.0.%d", i+1),
			fmt.Sprintf("zone-%02d", i%3),
		))
	}
	return &utils.PodArray{Pods: pods}
}

func benchmarkExternalPayloadSize(b *testing.B, cfg externalRouterConfig, pods types.PodList) int {
	b.Helper()
	ctx := types.NewRoutingContext(context.Background(), Algorithm, "benchmark-model", "", "benchmark-request", "")
	request, _, err := buildExternalDecisionRequest(cfg, nil, ctx, pods)
	if err != nil {
		b.Fatal(err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		b.Fatal(err)
	}
	return len(payload)
}

func BenchmarkBuildExternalDecisionRequest(b *testing.B) {
	for _, candidateCount := range []int{8, 32, 128} {
		b.Run(fmt.Sprintf("candidates=%d", candidateCount), func(b *testing.B) {
			cfg := externalRouterConfig{policyMode: PolicyAuthoritative, candidateMetrics: map[string]struct{}{}}
			pods := benchmarkExternalPods(candidateCount)
			ctx := types.NewRoutingContext(context.Background(), Algorithm, "benchmark-model", "", "benchmark-request", "")
			payloadBytes := benchmarkExternalPayloadSize(b, cfg, pods)
			b.SetBytes(int64(payloadBytes))
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				request, snapshots, err := buildExternalDecisionRequest(cfg, nil, ctx, pods)
				if err != nil {
					b.Fatal(err)
				}
				payload, err := json.Marshal(request)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkExternalPayload = payload
				benchmarkExternalSnapshotCount = len(snapshots)
			}
			b.ReportMetric(float64(payloadBytes), "payload_bytes/op")
		})
	}
}

func BenchmarkExternalRouterRoundTrip(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var request externalDecisionRequest
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(request.Spec.Candidates) == 0 {
			http.Error(w, "missing candidates", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", externalMediaType)
		target := request.Spec.Candidates[len(request.Spec.Candidates)-1].ID
		_, _ = io.WriteString(w, externalResponseFor(request.Metadata.RequestID, externalDecisionSelected, target))
	}))
	b.Cleanup(server.Close)

	for _, candidateCount := range []int{8, 32, 128} {
		b.Run(fmt.Sprintf("candidates=%d", candidateCount), func(b *testing.B) {
			cfg := externalRouterTestConfig(server.URL, PolicyAuthoritative, FailureFailClosed)
			cfg.maxInflight = 256
			client := newExternalHTTPClient(cfg)
			b.Cleanup(client.CloseIdleConnections)
			router := newExternalRouterWithDependencies(cfg, nil, client, nil, prometheus.NewRegistry())
			pods := benchmarkExternalPods(candidateCount)
			payloadBytes := benchmarkExternalPayloadSize(b, cfg, pods)
			b.SetBytes(int64(payloadBytes))
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				ctx := types.NewRoutingContext(context.Background(), Algorithm, "benchmark-model", "", "benchmark-request", "")
				address, err := router.Route(ctx, pods)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkExternalAddress = address
				ctx.Delete()
			}
			b.ReportMetric(float64(payloadBytes), "payload_bytes/op")
		})
	}
}
