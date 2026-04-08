/*
Copyright 2024 The Aibrix Team.

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
	"testing"
	"time"

	"github.com/go-redis/redismock/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
)

const testModelName = "test-model"

// TestRedisRequestCounter_AtomicDecrementDelete verifies that the Lua script
// atomically decrements and deletes, preventing race conditions
func TestRedisRequestCounter_AtomicDecrementDelete(t *testing.T) {
	// Create mock Redis client
	client, mock := redismock.NewClientMock()
	defer func() { _ = client.Close() }()

	counter := NewRedisRequestCounter(client)
	modelName := testModelName
	key := counter.buildRedisKey(modelName)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: modelName,
			},
		},
	}
	field := podServerKey{pod: pod}.String()

	// Test case 1: Decrement from 1 to 0 should delete the field
	t.Run("decrement_to_zero", func(t *testing.T) {
		// Expect any Eval call with key and field, return 0
		mock.Regexp().ExpectEval(`.*HINCRBY.*`, []string{key}, field).SetVal(int64(0))

		// Create context using constructor
		ctx := types.NewRoutingContext(context.Background(), "", modelName, "", "req1", "")
		ctx.SetTargetPod(pod)
		// Mark that AddRequestCount was called
		ctx.Context = context.WithValue(ctx.Context, requestCountAddedKey, true)

		// Call DoneRequestCount which should decrement to 0 and delete
		counter.DoneRequestCount(ctx, "req1", modelName, 1)

		// Verify all expectations were met
		require.NoError(t, mock.ExpectationsWereMet())
	})

	// Test case 2: Decrement from 0 to -1 should delete the field
	t.Run("decrement_to_negative", func(t *testing.T) {
		mock.Regexp().ExpectEval(`.*HINCRBY.*`, []string{key}, field).SetVal(int64(-1))

		ctx := types.NewRoutingContext(context.Background(), "", modelName, "", "req2", "")
		ctx.SetTargetPod(pod)
		ctx.Context = context.WithValue(ctx.Context, requestCountAddedKey, true)

		counter.DoneRequestCount(ctx, "req2", modelName, 0)

		require.NoError(t, mock.ExpectationsWereMet())
	})

	// Test case 3: Decrement from 5 to 4 should NOT delete the field
	t.Run("decrement_above_zero", func(t *testing.T) {
		// Expect Lua script returns 4 (count > 0, so no deletion happens in Lua)
		mock.Regexp().ExpectEval(`.*HINCRBY.*`, []string{key}, field).SetVal(int64(4))

		ctx := types.NewRoutingContext(context.Background(), "", modelName, "", "req3", "")
		ctx.SetTargetPod(pod)
		ctx.Context = context.WithValue(ctx.Context, requestCountAddedKey, true)

		counter.DoneRequestCount(ctx, "req3", modelName, 5)

		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestRedisRequestCounter_AddRequestCount verifies increment operation
func TestRedisRequestCounter_AddRequestCount(t *testing.T) {
	client, mock := redismock.NewClientMock()
	defer func() { _ = client.Close() }()

	counter := NewRedisRequestCounter(client)
	modelName := testModelName
	key := counter.buildRedisKey(modelName)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: modelName,
			},
		},
	}
	field := podServerKey{pod: pod}.String()

	t.Run("first_request_adds_count", func(t *testing.T) {
		// Update last route time to enable tracking
		counter.lastModelRouteTimeMu.Lock()
		counter.lastModelRouteTime[modelName] = time.Now()
		counter.lastModelRouteTimeMu.Unlock()

		// Expect HIncrBy to return 1 (first request)
		mock.ExpectHIncrBy(key, field, 1).SetVal(1)
		// Expect Expire to be called for TTL
		mock.ExpectExpire(key, counter.requestTrackerTimeout).SetVal(true)

		ctx := types.NewRoutingContext(context.Background(), "", modelName, "", "req1", "")
		ctx.SetTargetPod(pod)

		traceTerm := counter.AddRequestCount(ctx, "req1", modelName)
		assert.Equal(t, int64(1), traceTerm)

		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("second_request_increments_count", func(t *testing.T) {
		// Update last route time
		counter.lastModelRouteTimeMu.Lock()
		counter.lastModelRouteTime[modelName] = time.Now()
		counter.lastModelRouteTimeMu.Unlock()

		// Expect HIncrBy to return 2 (second request)
		mock.ExpectHIncrBy(key, field, 1).SetVal(2)
		// Expect Expire to be called for TTL
		mock.ExpectExpire(key, counter.requestTrackerTimeout).SetVal(true)

		ctx := types.NewRoutingContext(context.Background(), "", modelName, "", "req2", "")
		ctx.SetTargetPod(pod)

		traceTerm := counter.AddRequestCount(ctx, "req2", modelName)
		assert.Equal(t, int64(2), traceTerm)

		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("skip_if_already_called", func(t *testing.T) {
		// If AddRequestCount was already called, should skip
		ctx := types.NewRoutingContext(context.Background(), "", modelName, "", "req3", "")
		ctx.SetTargetPod(pod)
		// Mark as already added
		ctx.Context = context.WithValue(ctx.Context, requestCountAddedKey, true)

		traceTerm := counter.AddRequestCount(ctx, "req3", modelName)
		assert.Equal(t, int64(0), traceTerm)

		// No Redis expectations should be set - verify no calls were made
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestRedisRequestCounter_DoneRequestCount_Skips verifies that DoneRequestCount
// properly skips when AddRequestCount was not called
func TestRedisRequestCounter_DoneRequestCount_Skips(t *testing.T) {
	client, mock := redismock.NewClientMock()
	defer func() { _ = client.Close() }()

	counter := NewRedisRequestCounter(client)
	modelName := testModelName
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: modelName,
			},
		},
	}

	t.Run("skip_if_add_not_called", func(t *testing.T) {
		ctx := types.NewRoutingContext(context.Background(), "", modelName, "", "req1", "")
		ctx.SetTargetPod(pod)
		// Do NOT mark AddRequestCount as called

		// Call DoneRequestCount - should skip and not call Redis
		counter.DoneRequestCount(ctx, "req1", modelName, 1)

		// No Redis operations should have been performed
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("skip_if_done_already_called", func(t *testing.T) {
		ctx := types.NewRoutingContext(context.Background(), "", modelName, "", "req2", "")
		ctx.SetTargetPod(pod)
		// Mark both Add and Done as called
		ctx.Context = context.WithValue(ctx.Context, requestCountAddedKey, true)
		ctx.Context = context.WithValue(ctx.Context, requestCountDoneKey, true)

		// Call DoneRequestCount - should skip because already called
		counter.DoneRequestCount(ctx, "req2", modelName, 1)

		// No Redis operations should have been performed
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
