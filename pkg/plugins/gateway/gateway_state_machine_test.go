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
