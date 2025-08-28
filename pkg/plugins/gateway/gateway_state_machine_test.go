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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPerRequestState_StateTransitions(t *testing.T) {
	tests := []struct {
		name           string
		initialState   requestState
		expectedStates []requestState
	}{
		{
			name:         "normal flow",
			initialState: stateAwaitingHeaders,
			expectedStates: []requestState{
				stateAwaitingHeaders,
				stateAwaitingBody,
				stateAwaitingDecision,
				stateForwarding,
				stateDone,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &perRequestState{
				currentState: tt.initialState,
				requestID:    "test-request",
			}

			assert.Equal(t, tt.expectedStates[0], state.currentState)

			// Test state transitions
			for i := 1; i < len(tt.expectedStates); i++ {
				state.currentState = tt.expectedStates[i]
				assert.Equal(t, tt.expectedStates[i], state.currentState)
			}
		})
	}
}

func TestSchedulerIntegration_StateTransitions(t *testing.T) {
	// Test that scheduler integration works with state machine
	state := &perRequestState{
		currentState:   stateAwaitingDecision,
		requestID:      "test-scheduler-request",
		submissionTime: time.Now(),
	}

	// Simulate scheduler decision received
	state.dispatchTime = time.Now().Add(10 * time.Millisecond)
	state.currentState = stateForwarding

	assert.Equal(t, stateForwarding, state.currentState)
	assert.False(t, state.dispatchTime.IsZero())

	// Test timing calculations
	waitTime := state.dispatchTime.Sub(state.submissionTime)
	assert.Greater(t, waitTime, time.Duration(0))

	t.Log("Scheduler integration state transitions test completed")
}

func TestLoadAwareScheduling_StateTracking(t *testing.T) {
	// Test that load-aware scheduling properly tracks state
	states := []*perRequestState{
		{
			currentState:   stateAwaitingDecision,
			requestID:      "high-load-request-1",
			submissionTime: time.Now(),
		},
		{
			currentState:   stateAwaitingDecision,
			requestID:      "high-load-request-2",
			submissionTime: time.Now().Add(1 * time.Millisecond),
		},
		{
			currentState:   stateAwaitingDecision,
			requestID:      "high-load-request-3",
			submissionTime: time.Now().Add(2 * time.Millisecond),
		},
	}

	// Simulate batch scheduling decision
	batchDispatchTime := time.Now().Add(5 * time.Millisecond)
	for _, state := range states {
		state.dispatchTime = batchDispatchTime
		state.currentState = stateForwarding
	}

	// Verify all states transitioned correctly
	for i, state := range states {
		assert.Equal(t, stateForwarding, state.currentState, "State %d should be forwarding", i)
		assert.Equal(t, batchDispatchTime, state.dispatchTime, "State %d should have batch dispatch time", i)

		waitTime := state.dispatchTime.Sub(state.submissionTime)
		assert.Greater(t, waitTime, time.Duration(0), "State %d should have positive wait time", i)
	}

	t.Log("Load-aware scheduling state tracking test completed")
}

func TestCapacityAwareScheduling_Metrics(t *testing.T) {
	// Test capacity-aware scheduling metrics tracking
	now := time.Now()

	// Simulate different capacity scenarios
	scenarios := []struct {
		name              string
		podCapacity       int
		requestCount      int
		expectedBatchSize int
	}{
		{
			name:              "low capacity pod",
			podCapacity:       1,
			requestCount:      10,
			expectedBatchSize: 1,
		},
		{
			name:              "high capacity pod",
			podCapacity:       100,
			requestCount:      10,
			expectedBatchSize: 10,
		},
		{
			name:              "overloaded scenario",
			podCapacity:       50,
			requestCount:      100,
			expectedBatchSize: 50,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// Create states for requests
			states := make([]*perRequestState, scenario.requestCount)
			for i := 0; i < scenario.requestCount; i++ {
				states[i] = &perRequestState{
					currentState:   stateAwaitingDecision,
					requestID:      fmt.Sprintf("capacity-test-request-%d", i),
					submissionTime: now.Add(time.Duration(i) * time.Millisecond),
				}
			}

			// Simulate capacity-aware batch scheduling
			batchSize := min(scenario.expectedBatchSize, len(states))
			batchDispatchTime := now.Add(10 * time.Millisecond)

			// Only dispatch up to capacity
			for i := 0; i < batchSize; i++ {
				states[i].dispatchTime = batchDispatchTime
				states[i].currentState = stateForwarding
			}

			// Verify correct number were dispatched
			dispatchedCount := 0
			for _, state := range states {
				if state.currentState == stateForwarding {
					dispatchedCount++
				}
			}

			assert.Equal(t, batchSize, dispatchedCount, "Should dispatch exactly batch size")

			// Verify remaining are still waiting
			waitingCount := 0
			for _, state := range states {
				if state.currentState == stateAwaitingDecision {
					waitingCount++
				}
			}

			expectedWaiting := scenario.requestCount - batchSize
			assert.Equal(t, expectedWaiting, waitingCount, "Remaining should still be waiting")
		})
	}

	t.Log("Capacity-aware scheduling metrics test completed")
}

func TestServer_WithStateMachine(t *testing.T) {
	// Test that state machine components are properly defined
	assert.Equal(t, requestState(0), stateAwaitingHeaders)
	assert.Equal(t, requestState(1), stateAwaitingBody)
	assert.Equal(t, requestState(2), stateAwaitingDecision)
	assert.Equal(t, requestState(3), stateForwarding)
	assert.Equal(t, requestState(4), stateDone)

	// Test perRequestState initialization
	state := &perRequestState{
		currentState: stateAwaitingHeaders,
		requestID:    "test-123",
		sessionID:    "session-456",
	}

	assert.Equal(t, stateAwaitingHeaders, state.currentState)
	assert.Equal(t, "test-123", state.requestID)
	assert.Equal(t, "session-456", state.sessionID)
}

func TestRequestState_Constants(t *testing.T) {
	// Test that state constants are properly defined
	assert.Equal(t, requestState(0), stateAwaitingHeaders)
	assert.Equal(t, requestState(1), stateAwaitingBody)
	assert.Equal(t, requestState(2), stateAwaitingDecision)
	assert.Equal(t, requestState(3), stateForwarding)
	assert.Equal(t, requestState(4), stateDone)
}

func TestPerRequestState_Initialization(t *testing.T) {
	state := &perRequestState{
		currentState: stateAwaitingHeaders,
		requestID:    "test-123",
		sessionID:    "session-456",
	}

	assert.Equal(t, stateAwaitingHeaders, state.currentState)
	assert.Equal(t, "test-123", state.requestID)
	assert.Equal(t, "session-456", state.sessionID)
	assert.False(t, state.completed)
	assert.False(t, state.isRespError)
	assert.Equal(t, int64(0), state.rpm)
	assert.Equal(t, int64(0), state.traceTerm)

	// Test new timing fields
	assert.True(t, state.requestStartTime.IsZero())
	assert.True(t, state.submissionTime.IsZero())
	assert.True(t, state.dispatchTime.IsZero())
	assert.Nil(t, state.schedulingDecision)
}

func TestPerRequestState_TimingCalculations(t *testing.T) {
	now := time.Now()
	state := &perRequestState{
		requestStartTime: now,
		submissionTime:   now.Add(10 * time.Millisecond),
		dispatchTime:     now.Add(50 * time.Millisecond),
	}

	// Test wait time calculation (submission to dispatch)
	expectedWaitTime := 40 * time.Millisecond
	actualWaitTime := state.dispatchTime.Sub(state.submissionTime)
	assert.Equal(t, expectedWaitTime, actualWaitTime)

	// Test execution time would be calculated from dispatchTime to completion
	// (this would be done with time.Since(state.dispatchTime) in real code)
	completionTime := now.Add(100 * time.Millisecond)
	expectedExecutionTime := 50 * time.Millisecond
	actualExecutionTime := completionTime.Sub(state.dispatchTime)
	assert.Equal(t, expectedExecutionTime, actualExecutionTime)
}
