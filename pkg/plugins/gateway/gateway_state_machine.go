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

package gateway

import (
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/scheduler"
	"github.com/vllm-project/aibrix/pkg/types"
)

// ProcessStateMachine implements the state machine-based request processing with scheduler integration
func (s *Server) ProcessStateMachine(srv extProcPb.ExternalProcessor_ProcessServer) error {
	ctx := srv.Context()

	// Initialize the state for this specific request stream
	state := &perRequestState{
		currentState: stateAwaitingHeaders,
		requestID:    uuid.New().String(),
	}

	// This channel will receive the scheduler's decision without blocking the main loop
	// Buffer size of 1 is sufficient since we only expect one decision per request
	decisionChan := make(chan *scheduler.Decision, 1)

	// Channel to receive messages from Envoy in a non-blocking way
	// Buffer size of 10 to handle burst of messages in high load scenarios
	msgChan := make(chan *extProcPb.ProcessingRequest, 10)
	errChan := make(chan error, 1)

	klog.InfoS("new request stream started", "requestID", state.requestID)
	defer func() {
		klog.InfoS("request stream finished", "requestID", state.requestID)

		// CRITICAL: Ensure FinalizeJob is always called if a decision was made.
		if state.schedulingDecision != nil && !state.completed {
			klog.InfoS("finalizing job due to unexpected stream termination", "requestID", state.requestID)
			// Calculate approximate times
			if !state.dispatchTime.IsZero() {
				waitTime := state.dispatchTime.Sub(state.submissionTime)
				executionTime := time.Since(state.dispatchTime)
				inheritedCST := state.schedulingDecision.Job.InheritedCST
				s.scheduler.FinalizeJob(state.sessionID, inheritedCST, executionTime, waitTime)
			}
		}

		s.cache.DoneRequestCount(state.routerCtx, state.requestID, state.model, state.traceTerm)
		if state.routerCtx != nil {
			state.routerCtx.Delete()
		}
	}()

	// Start goroutine to receive messages from Envoy
	go func() {
		for {
			req, err := srv.Recv()
			// IMPORTANT: Check for context cancellation *before* sending to channel
			select {
			case <-ctx.Done():
				// The main loop will handle the error, just exit.
				return
			default:
			}

			if err != nil {
				errChan <- err
				return
			}
			msgChan <- req
		}
	}()

	for state.currentState != stateDone {
		select {
		case <-ctx.Done():
			klog.InfoS("request context cancelled", "requestID", state.requestID)
			return ctx.Err()

		case decision := <-decisionChan:
			// Event: Scheduler has made a decision
			if state.currentState != stateAwaitingDecision {
				klog.ErrorS(nil, "received scheduler decision in unexpected state", "requestID", state.requestID, "state", state.currentState)
				continue
			}

			klog.InfoS("received scheduling decision", "requestID", state.requestID, "sessionID", state.sessionID)
			state.schedulingDecision = decision
			state.dispatchTime = time.Now() // Record when scheduler granted permission

			if decision.Err != nil {
				// Handle scheduling failure
				errResp := generateErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
					[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
						Key: HeaderErrorRouting, RawValue: []byte("true")}}},
					"scheduler error")
				if err := srv.Send(errResp); err != nil {
					klog.ErrorS(err, "failed to send scheduler error response", "requestID", state.requestID)
				}
				state.currentState = stateDone
				continue
			}

			// We have permission to proceed. Handle the actual routing
			resp := s.handleScheduledRequest(state.routerCtx, state)
			if err := srv.Send(resp); err != nil {
				klog.ErrorS(err, "failed to send routing response", "requestID", state.requestID)
				state.currentState = stateDone
			} else {
				state.currentState = stateForwarding
			}

		case req := <-msgChan:
			// Event: Received a message from Envoy
			if err := s.handleEnvoyMessage(srv, req, state, decisionChan); err != nil {
				klog.ErrorS(err, "failed to handle envoy message", "requestID", state.requestID)
				return err
			}

		case err := <-errChan:
			// Event: Error receiving from Envoy
			if err == io.EOF {
				klog.InfoS("envoy stream closed", "requestID", state.requestID)
				state.currentState = stateDone
			} else {
				klog.ErrorS(err, "stream recv error", "requestID", state.requestID)
				return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
			}
		}
	}

	return nil
}

// handleEnvoyMessage processes messages received from Envoy based on current state
func (s *Server) handleEnvoyMessage(srv extProcPb.ExternalProcessor_ProcessServer, req *extProcPb.ProcessingRequest, state *perRequestState, decisionChan chan<- *scheduler.Decision) error {
	switch v := req.Request.(type) {
	case *extProcPb.ProcessingRequest_RequestHeaders:
		return s.handleRequestHeaders(srv, req, state)

	case *extProcPb.ProcessingRequest_RequestBody:
		return s.handleRequestBody(srv, req, state, decisionChan)

	case *extProcPb.ProcessingRequest_ResponseHeaders:
		return s.handleResponseHeaders(srv, req, state)

	case *extProcPb.ProcessingRequest_ResponseBody:
		return s.handleResponseBody(srv, req, state)

	default:
		klog.InfoS("unknown request type", "requestID", state.requestID, "type", v)
		return nil
	}
}

// handleRequestHeaders processes RequestHeaders message
func (s *Server) handleRequestHeaders(srv extProcPb.ExternalProcessor_ProcessServer, req *extProcPb.ProcessingRequest, state *perRequestState) error {
	if state.currentState != stateAwaitingHeaders {
		klog.ErrorS(nil, "received RequestHeaders in unexpected state", "requestID", state.requestID, "state", state.currentState)
		return nil
	}

	state.requestStartTime = time.Now()
	_, user, rpm, routerCtx := s.HandleRequestHeaders(srv.Context(), state.requestID, req)
	state.user, state.rpm, state.routerCtx = user, rpm, routerCtx

	// Extract Session ID as early as possible from headers
	if routerCtx != nil {
		state.sessionID = extractSessionID(state.requestID, routerCtx.ReqPath, nil, routerCtx.ReqHeaders)
		klog.InfoS("extracted session ID from headers", "requestID", state.requestID, "sessionID", state.sessionID)
	}

	// Don't send response yet, wait for body
	state.currentState = stateAwaitingBody
	return nil
}

// handleRequestBody processes RequestBody message and initiates scheduling
func (s *Server) handleRequestBody(srv extProcPb.ExternalProcessor_ProcessServer, req *extProcPb.ProcessingRequest, state *perRequestState, decisionChan chan<- *scheduler.Decision) error {
	if state.currentState != stateAwaitingBody {
		klog.ErrorS(nil, "received RequestBody in unexpected state", "requestID", state.requestID, "state", state.currentState)
		return nil
	}

	resp, model, routerCtx, stream, traceTerm := s.HandleRequestBody(srv.Context(), state.requestID, req, state.user)
	state.model, state.routerCtx, state.stream, state.traceTerm = model, routerCtx, stream, traceTerm

	if resp != nil {
		// Handle cases where HandleRequestBody returns an error response
		if err := srv.Send(resp); err != nil {
			klog.ErrorS(err, "failed to send request body error response", "requestID", state.requestID)
		}
		state.currentState = stateDone
		return nil
	}

	// Refine session ID with body information if needed
	if state.sessionID == "" || state.sessionID == state.requestID {
		state.sessionID = extractSessionID(state.requestID, routerCtx.ReqPath, routerCtx.ReqBody, routerCtx.ReqHeaders)
	}

	// Submit job to scheduler asynchronously with timeout protection
	state.submissionTime = time.Now()
	go func() {
		klog.InfoS("submitting job to scheduler", "requestID", state.requestID, "sessionID", state.sessionID)

		// TODO: Add timeout protection when scheduler supports context
		decision, err := s.scheduler.SubmitJob(state.routerCtx, state.sessionID)
		if err != nil {
			decision = &scheduler.Decision{Err: err}
		}

		// Non-blocking send to avoid goroutine leak if main loop exits
		select {
		case decisionChan <- decision:
		case <-srv.Context().Done():
			// Main context cancelled, don't send decision
		}
	}()

	state.currentState = stateAwaitingDecision
	return nil
}

// handleResponseHeaders processes ResponseHeaders message
func (s *Server) handleResponseHeaders(srv extProcPb.ExternalProcessor_ProcessServer, req *extProcPb.ProcessingRequest, state *perRequestState) error {
	if state.currentState != stateForwarding {
		klog.ErrorS(nil, "received ResponseHeaders in unexpected state", "requestID", state.requestID, "state", state.currentState)
		return nil
	}

	resp, isRespError, respErrorCode := s.HandleResponseHeaders(srv.Context(), state.requestID, state.model, req)
	state.isRespError, state.respErrorCode = isRespError, respErrorCode

	if isRespError && respErrorCode == 500 {
		// for error code 500, ProcessingRequest_ResponseBody is not invoked
		resp = s.responseErrorProcessing(srv.Context(), resp, respErrorCode, state.model, state.requestID, "")
	}

	if isRespError && respErrorCode == 401 {
		// Early return due to unauthorized or canceled context
		resp = s.responseErrorProcessing(srv.Context(), resp, respErrorCode, state.model, state.requestID, `{"error":"unauthorized"}`)
	}

	if err := srv.Send(resp); err != nil {
		klog.ErrorS(err, "failed to send response headers", "requestID", state.requestID)
		state.currentState = stateDone
	}

	return nil
}

// handleResponseBody processes ResponseBody message and finalizes the job
func (s *Server) handleResponseBody(srv extProcPb.ExternalProcessor_ProcessServer, req *extProcPb.ProcessingRequest, state *perRequestState) error {
	if state.currentState != stateForwarding {
		klog.ErrorS(nil, "received ResponseBody in unexpected state", "requestID", state.requestID, "state", state.currentState)
		return nil
	}

	var resp *extProcPb.ProcessingResponse
	if state.isRespError {
		resp = s.responseErrorProcessing(srv.Context(), resp, state.respErrorCode, state.model, state.requestID,
			string(req.Request.(*extProcPb.ProcessingRequest_ResponseBody).ResponseBody.GetBody()))
	} else {
		resp, state.completed = s.HandleResponseBody(srv.Context(), state.requestID, req, state.user, state.rpm, state.model, state.stream, state.traceTerm, state.completed)
	}

	// Finalize the job upon completion
	if state.completed && state.sessionID != "" && state.schedulingDecision != nil {
		// WaitTime is the duration from submission to dispatch.
		waitTime := state.dispatchTime.Sub(state.submissionTime)
		// ExecutionTime is the duration from dispatch to completion.
		executionTime := time.Since(state.dispatchTime)
		inheritedCST := state.schedulingDecision.Job.InheritedCST

		klog.InfoS("finalizing job", "requestID", state.requestID, "sessionID", state.sessionID,
			"executionTime", executionTime, "waitTime", waitTime, "inheritedCST", inheritedCST)

		s.scheduler.FinalizeJob(state.sessionID, inheritedCST, executionTime, waitTime)
		state.currentState = stateDone
	}

	if err := srv.Send(resp); err != nil {
		klog.ErrorS(err, "failed to send response body", "requestID", state.requestID)
		state.currentState = stateDone
	}

	return nil
}

// handleScheduledRequest performs routing after receiving scheduler permission
func (s *Server) handleScheduledRequest(routingCtx *types.RoutingContext, state *perRequestState) *extProcPb.ProcessingResponse {
	// Get affinity hint from the session cache
	if sessionState, exists := s.sessionCache.GetState(state.sessionID); exists && sessionState.PodAffinity != "" {
		klog.InfoS("using pod affinity hint", "requestID", state.requestID, "sessionID", state.sessionID, "podAffinity", sessionState.PodAffinity)
		// TODO: Use pod affinity hint in routing decision
	}

	// Get pods for the model
	podsArr, err := s.cache.ListPodsByModel(state.model)
	if err != nil || podsArr == nil || len(podsArr.All()) == 0 {
		klog.ErrorS(err, "no ready pod available", "requestID", state.requestID, "model", state.model)
		return generateErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorNoModelBackends, RawValue: []byte("true")}}},
			"no ready pods available")
	}

	// Perform routing
	targetPodIP, err := s.selectTargetPod(routingCtx, podsArr)
	if targetPodIP == "" || err != nil {
		klog.ErrorS(err, "failed to select target pod", "requestID", state.requestID, "model", state.model)
		return generateErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorRouting, RawValue: []byte("true")}}},
			"error on selecting target pod")
	}

	// Update session cache with pod affinity
	if routingCtx.HasRouted() {
		podName := routingCtx.TargetPod().Name
		s.sessionCache.UpdateAffinity(state.sessionID, podName)
	}

	// Build response headers
	headers := buildEnvoyProxyHeaders([]*configPb.HeaderValueOption{},
		HeaderRoutingStrategy, string(routingCtx.Algorithm),
		HeaderTargetPod, targetPodIP,
		"content-length", strconv.Itoa(len(routingCtx.ReqBody)))

	klog.InfoS("request start", "requestID", state.requestID, "requestPath", routingCtx.ReqPath, "model", state.model, "stream", state.stream, "routingAlgorithm", routingCtx.Algorithm, "targetPodIP", targetPodIP, "routingDuration", routingCtx.GetRoutingDelay())

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					BodyMutation: &extProcPb.BodyMutation{
						Mutation: &extProcPb.BodyMutation_Body{
							Body: routingCtx.ReqBody,
						},
					},
				},
			},
		},
	}
}
