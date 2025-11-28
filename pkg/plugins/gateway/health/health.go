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
	"time"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	LivenessCheckService  = "liveness"
	ReadinessCheckService = "readiness"
)

// SimpleHealthServer implements health check with Leader Election support
type SimpleHealthServer struct {
	// Leader election related
	isLeader              *atomic.Bool
	leaderElectionEnabled bool
}

func NewHealthServer(isLeader *atomic.Bool, leaderElectionEnabled bool) *SimpleHealthServer {
	return &SimpleHealthServer{
		isLeader:              isLeader,
		leaderElectionEnabled: leaderElectionEnabled,
	}
}

func (s *SimpleHealthServer) Check(ctx context.Context, in *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	klog.V(4).Infof("Health check request for service: %s, leader election enabled: %t, current leader: %t",
		in.Service, s.leaderElectionEnabled, s.isLeader.Load())

	// If leader election is not enabled, return SERVING for all services (compatibility)
	if !s.leaderElectionEnabled {
		klog.V(6).Infof("Leader election disabled, returning SERVING for service: %s", in.Service)
		return &healthpb.HealthCheckResponse{
			Status: healthpb.HealthCheckResponse_SERVING,
		}, nil
	}

	// when leader election is enabled
	switch in.Service {
	case LivenessCheckService:
		// Liveness: any running instance returns SERVING
		// This prevents non-leader pods from being restarted due to readiness failure
		klog.V(6).Infof("Liveness check for service: %s, returning SERVING", in.Service)
		return &healthpb.HealthCheckResponse{
			Status: healthpb.HealthCheckResponse_SERVING,
		}, nil
	case ReadinessCheckService, "":
		// Readiness and empty service name: only leader returns SERVING
		// This ensures only leader is added to Kubernetes Service Endpoints
		if s.isLeader.Load() {
			klog.V(6).Infof("Readiness check for service: %s, current instance is leader, returning SERVING", in.Service)
			return &healthpb.HealthCheckResponse{
				Status: healthpb.HealthCheckResponse_SERVING,
			}, nil
		}
		klog.V(6).Infof("Readiness check for service: %s, current instance is not leader, returning NOT_SERVING", in.Service)
		return &healthpb.HealthCheckResponse{
			Status: healthpb.HealthCheckResponse_NOT_SERVING,
		}, nil
	default:
		// Other services (e.g., gateway): only leader returns SERVING
		if s.isLeader.Load() {
			klog.V(6).Infof("Health check for service: %s, current instance is leader, returning SERVING", in.Service)
			return &healthpb.HealthCheckResponse{
				Status: healthpb.HealthCheckResponse_SERVING,
			}, nil
		}
		klog.V(6).Infof("Health check for service: %s, current instance is not leader, returning NOT_SERVING", in.Service)
		return &healthpb.HealthCheckResponse{
			Status: healthpb.HealthCheckResponse_NOT_SERVING,
		}, nil
	}
}

func (s *SimpleHealthServer) Watch(in *healthpb.HealthCheckRequest, stream healthpb.Health_WatchServer) error {
	klog.V(6).Infof("Health watch request for service: %s", in.Service)

	// Simple implementation: send current status periodically
	update := make(chan healthpb.HealthCheckResponse_ServingStatus, 1)

	// Send initial status
	initialResp, err := s.Check(context.Background(), in)
	if err != nil {
		klog.Errorf("Failed to get initial status for service %s: %v", in.Service, err)
		return err
	}
	update <- initialResp.Status
	klog.V(6).Infof("Sent initial status %s for service: %s", initialResp.Status.String(), in.Service)

	// Update status periodically
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				resp, err := s.Check(context.Background(), in)
				if err != nil {
					klog.Errorf("Failed to get status for service %s: %v", in.Service, err)
					return
				}
				select {
				case update <- resp.Status:
					klog.V(6).Infof("Updated status %s for service: %s", resp.Status.String(), in.Service)
				default:
					// Channel full, skip
					klog.V(6).Infof("Status channel full, skipping update for service: %s", in.Service)
				}
			case <-stream.Context().Done():
				klog.V(6).Infof("Stream context done for service: %s", in.Service)
				return
			}
		}
	}()

	var lastSentStatus healthpb.HealthCheckResponse_ServingStatus = -1
	for {
		select {
		case servingStatus := <-update:
			if lastSentStatus != servingStatus {
				err := stream.Send(&healthpb.HealthCheckResponse{Status: servingStatus})
				if err != nil {
					klog.Errorf("Failed to send health status for service %s: %v", in.Service, err)
					return status.Error(codes.Canceled, "Stream has ended.")
				}
				klog.V(6).Infof("Sent status %s for service: %s", servingStatus.String(), in.Service)
				lastSentStatus = servingStatus
			}
		case <-stream.Context().Done():
			klog.V(6).Infof("Stream context done for service: %s", in.Service)
			return status.Error(codes.Canceled, "Stream has ended.")
		}
	}
}
