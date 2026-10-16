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

package gateway

import (
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/metrics"
)

// ProcessFullMethod is the ExtProc method Envoy calls for every request. Process
// reports it when it recovers a panic on its own, without the stream interceptor
// that normally wraps it.
const ProcessFullMethod = "/envoy.service.ext_proc.v3.ExternalProcessor/Process"

// StreamPanicRecoveryInterceptor recovers panics raised while a stream handler
// runs. Without it a single defective request ends the gateway plugin process
// and every request in flight on it; with it the stream that panicked fails with
// codes.Internal and the rest of the server keeps serving.
//
// Recovering here is a last line of defense, not a replacement for fixing the
// defect: the stack trace is logged and the panic is counted, so the condition
// stays visible instead of being swallowed.
func StreamPanicRecoveryInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = recoverStreamPanic(r, methodOf(info))
			}
		}()
		return handler(srv, stream)
	}
}

// recoverStreamPanic logs the stack trace, counts the panic and returns the
// status the panicking stream is closed with. The panic value stays in the log:
// the reply only names the method, the way other internal errors on the request
// path do.
func recoverStreamPanic(r any, method string) error {
	klog.Errorf("gateway plugin recovered from a panic on %s: %v\n%s", method, r, debug.Stack())
	metrics.EmitMetricToPrometheus(
		nil,
		nil,
		metrics.GatewayRequestPanicTotal,
		&metrics.SimpleMetricValue{Value: 1},
		map[string]string{"pod_name": podName},
	)
	return status.Errorf(codes.Internal, "gateway plugin recovered from an internal error while handling %s", method)
}

// methodOf names the stream being handled, falling back to the ExtProc method
// when the caller did not go through the server's transport info.
func methodOf(info *grpc.StreamServerInfo) string {
	if info == nil || info.FullMethod == "" {
		return ProcessFullMethod
	}
	return info.FullMethod
}
