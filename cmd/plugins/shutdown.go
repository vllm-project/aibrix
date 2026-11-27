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

package main

import (
	"time"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"k8s.io/klog/v2"
)

func shutdownGateway(
	server *grpc.Server, gatewayServer *gateway.Server, healthCheck *health.Server, timeout time.Duration,
) {
	healthCheck.Shutdown()
	// Keep the gateway's shutdown channel, HTTP listener and request dependencies
	// alive while gRPC rejects new RPCs and lets existing streams finish.
	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		klog.Info("gRPC requests drained; stopping gateway services")
	case <-timer.C:
		klog.Warningf("gRPC drain deadline reached after %s; aborting remaining requests", timeout)
		server.Stop()
		<-done
	}
	gatewayServer.Shutdown()
}
