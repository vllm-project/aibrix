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

package trafficdrain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/internal/trafficdraintest"
)

func TestAdmissionAndSealAreCoordinated(t *testing.T) {
	_, client, _ := trafficdraintest.NewRedis(t)
	first, second := New(client), New(client)
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })
	ctx := context.Background()
	epoch, err := first.Initialize(ctx, "pod")
	require.NoError(t, err)
	require.NoError(t, first.Admit(ctx, "pod", epoch, "request-one"))
	require.NoError(t, first.Admit(ctx, "pod", epoch, "request-one"), "retry is idempotent")
	snapshot, err := second.Start(ctx, "pod", epoch, time.Minute)
	require.NoError(t, err)
	require.EqualValues(t, 1, snapshot.Active)
	require.ErrorIs(t, second.Admit(ctx, "pod", epoch, "request-two"), ErrClosed)
	_, err = second.Seal(ctx, "pod", epoch, snapshot.Generation)
	require.ErrorIs(t, err, ErrConflict)
	require.NoError(t, first.Release(ctx, "pod", epoch, "request-one"))
	require.NoError(t, first.Release(ctx, "pod", epoch, "request-one"))
	sealed, err := second.Seal(ctx, "pod", epoch, snapshot.Generation)
	require.NoError(t, err)
	require.Equal(t, Sealed, sealed.Phase)
	_, err = second.Cancel(ctx, "pod", epoch, snapshot.Generation, 0)
	require.ErrorIs(t, err, ErrConflict, "a successful seal cannot be reopened before deletion")
	require.ErrorIs(t, first.Admit(ctx, "pod", epoch, "request-three"), ErrClosed)
	require.NoError(t, second.Forget(ctx, "pod", epoch))
	require.ErrorIs(t, first.Admit(ctx, "pod", epoch, "request-three"), ErrUnknown)
}

func TestMissingStateAndWrongEpochFailClosed(t *testing.T) {
	m, client, _ := trafficdraintest.NewRedis(t)
	s := New(client)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	epoch, err := s.Initialize(ctx, "pod")
	require.NoError(t, err)
	require.NoError(t, s.Admit(ctx, "pod", epoch, "running"))
	m.FlushAll()
	_, err = s.Read(ctx, "pod", epoch)
	require.ErrorIs(t, err, ErrUnknown)
	require.ErrorIs(t, s.Admit(ctx, "pod", epoch, "next"), ErrUnknown)
	require.ErrorIs(t, s.Release(ctx, "pod", epoch, "running"), ErrUnknown)
	require.Empty(t, m.Keys(), "a missing counter is never recreated by completion")
	_, err = s.Initialize(ctx, "replacement")
	require.NoError(t, err)
	require.Error(t, s.Admit(ctx, "replacement", epoch, "wrong-generation"))
}

func TestTimeoutAndOldAttemptCannotSeal(t *testing.T) {
	m, client, _ := trafficdraintest.NewRedis(t)
	now := time.Now()
	m.SetTime(now)
	s := New(client)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	epoch, err := s.Initialize(ctx, "pod")
	require.NoError(t, err)
	first, err := s.Start(ctx, "pod", epoch, time.Second)
	require.NoError(t, err)
	m.SetTime(now.Add(2 * time.Second))
	_, err = s.Seal(ctx, "pod", epoch, first.Generation)
	require.ErrorIs(t, err, ErrExpired)
	_, err = s.Cancel(ctx, "pod", epoch, first.Generation, time.Second)
	require.NoError(t, err)
	require.NoError(t, s.Admit(ctx, "pod", epoch, "after-cancel"))
	_, err = s.Start(ctx, "pod", epoch, time.Second)
	require.ErrorIs(t, err, ErrConflict)
	m.SetTime(now.Add(4 * time.Second))
	second, err := s.Start(ctx, "pod", epoch, time.Second)
	require.NoError(t, err)
	require.Greater(t, second.Generation, first.Generation)
	require.NoError(t, s.Release(ctx, "pod", epoch, "after-cancel"))
	_, err = s.Seal(ctx, "pod", epoch, first.Generation)
	require.ErrorIs(t, err, ErrConflict)
	_, err = s.Seal(ctx, "pod", epoch, second.Generation)
	require.NoError(t, err)
}

func TestRedisRestartCannotReuseRestoredState(t *testing.T) {
	m, client, id := trafficdraintest.NewRedis(t)
	s := New(client)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	epoch, err := s.Initialize(ctx, "pod")
	require.NoError(t, err)
	addr := m.Addr()
	m.Close()
	require.NoError(t, m.StartAddr(addr))
	id.Store(strings.Repeat("b", 40))
	trafficdraintest.SetIdentity(m, id)
	_, err = s.Read(ctx, "pod", epoch)
	require.Error(t, err)
	restarted := New(client)
	t.Cleanup(func() { _ = restarted.Close() })
	_, err = restarted.Read(ctx, "pod", epoch)
	require.ErrorIs(t, err, ErrRestarted)
	newEpoch, err := restarted.Initialize(ctx, "replacement")
	require.NoError(t, err)
	require.NoError(t, restarted.Admit(ctx, "replacement", newEpoch, "new-request"))
}
