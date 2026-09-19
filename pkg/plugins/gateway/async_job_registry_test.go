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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/vllm-project/aibrix/pkg/utils"
)

// fakeClock drives the stores' and registry's notion of "now" so expiry is
// asserted deterministically instead of by sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// asyncJobStoreFixture lets every store-contract test run unchanged against
// both the in-memory store (unit tests, local single-replica construction) and
// the Redis store (production). advance moves both the logical clock and, for
// Redis, the server's TTL clock.
type asyncJobStoreFixture struct {
	name    string
	store   asyncJobStore
	clock   *fakeClock
	advance func(time.Duration)
}

func newTestAsyncJobRedisClient(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

func asyncJobStoreFixtures(t *testing.T) []asyncJobStoreFixture {
	t.Helper()

	memClock := newFakeClock()
	memStore := newInMemoryAsyncJobStore()
	memStore.now = memClock.Now

	redisClock := newFakeClock()
	client, mr := newTestAsyncJobRedisClient(t)
	redisStore := newRedisAsyncJobStore(client)
	redisStore.now = redisClock.Now

	return []asyncJobStoreFixture{
		{
			name:    "memory",
			store:   memStore,
			clock:   memClock,
			advance: memClock.advance,
		},
		{
			name:  "redis",
			store: redisStore,
			clock: redisClock,
			advance: func(d time.Duration) {
				redisClock.advance(d)
				mr.FastForward(d)
			},
		},
	}
}

func testAsyncJobRecord(clock *fakeClock, owner, publicID, backendID string) AsyncJobRecord {
	now := clock.Now()
	return AsyncJobRecord{
		PublicJobID:  publicID,
		JobType:      asyncJobTypeVideo,
		Owner:        owner,
		Model:        "wan2.1",
		BackendJobID: backendID,
		RoutingTarget: AsyncJobRoutingTarget{
			Kind: asyncJobTargetKindPod,
			Pod: AsyncJobPodTarget{
				Namespace: "ns-a",
				Name:      "pod-a",
				UID:       "uid-pod-a",
			},
		},
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
}

// asyncJobReadyPod is local to the Registry contract tests: the Registry pins
// a pod identity, so its fixtures must include a UID.
func asyncJobReadyPod(name, namespace, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: k8stypes.UID("uid-" + namespace + "-" + name)},
		Status: v1.PodStatus{
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func TestAsyncJobOwnerScope(t *testing.T) {
	// The static bearer token carries no principal, so a request without a user
	// header shares one scope rather than being attributed to the token.
	assert.Equal(t, "scope:user:alice", asyncJobOwnerFromUser(utils.User{Name: "alice"}))
	assert.Equal(t, asyncJobOwnerShared, asyncJobOwnerFromUser(utils.User{}))
	assert.Equal(t, asyncJobOwnerShared, asyncJobOwnerFromUserName(""))
	assert.Equal(t, asyncJobOwnerShared, asyncJobOwnerFromUserName("   "))
	assert.Equal(t, "scope:user:bob", asyncJobOwnerFromUserName("bob"))
}

func TestNewAsyncJobPublicID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := newAsyncJobPublicID()
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(id, asyncJobPublicIDPrefix), "public id %q must be recognizable as AIBrix-generated", id)
		assert.Len(t, strings.TrimPrefix(id, asyncJobPublicIDPrefix), 2*asyncJobPublicIDBytes)
		assert.False(t, seen[id], "public ids must not repeat")
		seen[id] = true
	}
}

func TestAsyncJobInvalidRecordErrorCanBeWrappedWithoutExposingSentinel(t *testing.T) {
	_, err := normalizeAsyncJobListOptions(AsyncJobListOptions{Limit: 0})
	require.Error(t, err)

	wrapped := fmt.Errorf("parse video list options: %w", err)
	assert.ErrorIs(t, wrapped, errAsyncJobInvalidRecord)
	assert.Equal(t, "parse video list options: list limit must be between 1 and 100", wrapped.Error())
	assert.NotContains(t, wrapped.Error(), errAsyncJobInvalidRecord.Error())
}

func TestAsyncJobRecordValidate(t *testing.T) {
	clock := newFakeClock()
	valid := testAsyncJobRecord(clock, asyncJobOwnerShared, "job-1", "backend-1")
	require.NoError(t, valid.validate())

	tests := []struct {
		name   string
		mutate func(*AsyncJobRecord)
	}{
		{"missing public id", func(r *AsyncJobRecord) { r.PublicJobID = "" }},
		{"missing job type", func(r *AsyncJobRecord) { r.JobType = "" }},
		{"missing owner", func(r *AsyncJobRecord) { r.Owner = "" }},
		{"missing backend id", func(r *AsyncJobRecord) { r.BackendJobID = "" }},
		{"unsupported target kind", func(r *AsyncJobRecord) { r.RoutingTarget.Kind = "service" }},
		{"missing pod name", func(r *AsyncJobRecord) { r.RoutingTarget.Pod.Name = "" }},
		{"missing pod namespace", func(r *AsyncJobRecord) { r.RoutingTarget.Pod.Namespace = "" }},
		{"missing pod uid", func(r *AsyncJobRecord) { r.RoutingTarget.Pod.UID = "" }},
		{"missing expiry", func(r *AsyncJobRecord) { r.ExpiresAt = time.Time{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := valid
			tt.mutate(&record)
			assert.ErrorIs(t, record.validate(), errAsyncJobInvalidRecord)
		})
	}
}

func TestAsyncJobStore_PutGetListDelete(t *testing.T) {
	for _, fixture := range asyncJobStoreFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			record := testAsyncJobRecord(fixture.clock, asyncJobOwnerShared, "job-1", "backend-1")
			require.NoError(t, fixture.store.put(ctx, record))

			got, err := fixture.store.get(ctx, asyncJobOwnerShared, "job-1")
			require.NoError(t, err)
			assert.Equal(t, record.PublicJobID, got.PublicJobID)
			assert.Equal(t, record.BackendJobID, got.BackendJobID)
			assert.Equal(t, record.JobType, got.JobType)
			assert.Equal(t, record.Owner, got.Owner)
			assert.Equal(t, record.Model, got.Model)
			assert.Equal(t, record.RoutingTarget, got.RoutingTarget)
			assert.True(t, record.ExpiresAt.Equal(got.ExpiresAt), "want %v got %v", record.ExpiresAt, got.ExpiresAt)
			assert.True(t, record.CreatedAt.Equal(got.CreatedAt), "want %v got %v", record.CreatedAt, got.CreatedAt)

			listed, err := fixture.store.list(ctx, asyncJobOwnerShared, asyncJobTypeVideo)
			require.NoError(t, err)
			require.Len(t, listed, 1)
			assert.Equal(t, "job-1", listed[0].PublicJobID)

			require.NoError(t, fixture.store.delete(ctx, asyncJobOwnerShared, "job-1"))

			_, err = fixture.store.get(ctx, asyncJobOwnerShared, "job-1")
			assert.ErrorIs(t, err, errAsyncJobNotFound)

			// The secondary index must go with the record, not linger as a
			// dangling member that List would have to keep pruning.
			listed, err = fixture.store.list(ctx, asyncJobOwnerShared, asyncJobTypeVideo)
			require.NoError(t, err)
			assert.Empty(t, listed)

			// Deleting an already-deleted job is a no-op, so the DELETE cleanup
			// path can be retried safely.
			assert.NoError(t, fixture.store.delete(ctx, asyncJobOwnerShared, "job-1"))
		})
	}
}

func TestAsyncJobStore_GetUnknownJobIsNotFound(t *testing.T) {
	for _, fixture := range asyncJobStoreFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			_, err := fixture.store.get(context.Background(), asyncJobOwnerShared, "nope")
			assert.ErrorIs(t, err, errAsyncJobNotFound)
		})
	}
}

func TestAsyncJobStore_ScopeIsolation(t *testing.T) {
	for _, fixture := range asyncJobStoreFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			alice := asyncJobOwnerFromUserName("alice")
			bob := asyncJobOwnerFromUserName("bob")

			require.NoError(t, fixture.store.put(ctx, testAsyncJobRecord(fixture.clock, alice, "job-alice", "backend-alice")))
			require.NoError(t, fixture.store.put(ctx, testAsyncJobRecord(fixture.clock, asyncJobOwnerShared, "job-shared", "backend-shared")))

			// Another user's job is indistinguishable from a job that never existed.
			_, err := fixture.store.get(ctx, bob, "job-alice")
			assert.ErrorIs(t, err, errAsyncJobNotFound)

			// scope:shared is a scope of its own, not a wildcard over user scopes.
			_, err = fixture.store.get(ctx, asyncJobOwnerShared, "job-alice")
			assert.ErrorIs(t, err, errAsyncJobNotFound)
			_, err = fixture.store.get(ctx, alice, "job-shared")
			assert.ErrorIs(t, err, errAsyncJobNotFound)

			listed, err := fixture.store.list(ctx, alice, asyncJobTypeVideo)
			require.NoError(t, err)
			require.Len(t, listed, 1)
			assert.Equal(t, "job-alice", listed[0].PublicJobID)

			listed, err = fixture.store.list(ctx, bob, asyncJobTypeVideo)
			require.NoError(t, err)
			assert.Empty(t, listed)

			// A foreign owner cannot delete somebody else's record either.
			require.NoError(t, fixture.store.delete(ctx, bob, "job-alice"))
			_, err = fixture.store.get(ctx, alice, "job-alice")
			assert.NoError(t, err)
		})
	}
}

func TestAsyncJobStore_ListFiltersByJobType(t *testing.T) {
	for _, fixture := range asyncJobStoreFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			video := testAsyncJobRecord(fixture.clock, asyncJobOwnerShared, "job-video", "backend-video")
			other := testAsyncJobRecord(fixture.clock, asyncJobOwnerShared, "job-other", "backend-other")
			other.JobType = "some-other-type"

			require.NoError(t, fixture.store.put(ctx, video))
			require.NoError(t, fixture.store.put(ctx, other))

			listed, err := fixture.store.list(ctx, asyncJobOwnerShared, asyncJobTypeVideo)
			require.NoError(t, err)
			require.Len(t, listed, 1)
			assert.Equal(t, "job-video", listed[0].PublicJobID)
		})
	}
}

func TestAsyncJobStore_ExpiredRecordsAreAbsent(t *testing.T) {
	for _, fixture := range asyncJobStoreFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			record := testAsyncJobRecord(fixture.clock, asyncJobOwnerShared, "job-1", "backend-1")
			require.NoError(t, fixture.store.put(ctx, record))

			fixture.advance(2 * time.Hour)

			_, err := fixture.store.get(ctx, asyncJobOwnerShared, "job-1")
			assert.ErrorIs(t, err, errAsyncJobNotFound)

			listed, err := fixture.store.list(ctx, asyncJobOwnerShared, asyncJobTypeVideo)
			require.NoError(t, err)
			assert.Empty(t, listed)
		})
	}
}

// TestAsyncJobStore_RepeatedPutIsIdempotent covers the retry case: the same
// public job id written twice (a transient failure that actually landed, then
// retried) must leave exactly one record and one index member, not a duplicate
// that List would report twice.
func TestAsyncJobStore_RepeatedPutIsIdempotent(t *testing.T) {
	for _, fixture := range asyncJobStoreFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			record := testAsyncJobRecord(fixture.clock, asyncJobOwnerShared, "job-1", "backend-1")
			require.NoError(t, fixture.store.put(ctx, record))
			require.NoError(t, fixture.store.put(ctx, record))
			require.NoError(t, fixture.store.put(ctx, record))

			listed, err := fixture.store.list(ctx, asyncJobOwnerShared, asyncJobTypeVideo)
			require.NoError(t, err)
			assert.Len(t, listed, 1)
		})
	}
}

func TestRedisAsyncJobStore_Keys(t *testing.T) {
	assert.Equal(t, "aibrix:gateway:async_job:{scope:shared}:record:job-1", asyncJobRecordKey(asyncJobOwnerShared, "job-1"))
	assert.Equal(t, "aibrix:gateway:async_job:{scope:shared}:record:", asyncJobRecordKeyPrefix(asyncJobOwnerShared))
	assert.Equal(t, "aibrix:gateway:async_job:{scope:user:alice}:index:video", asyncJobIndexKey("scope:user:alice", asyncJobTypeVideo))
	assert.Equal(t, "aibrix:gateway:async_job:{scope:user:alice}:indexes", asyncJobIndexesKey("scope:user:alice"))
}

func TestIsValidPublicJobIDRejectsUnsafeCharacters(t *testing.T) {
	assert.True(t, isValidPublicJobID("aibrixjob-0123456789abcdef"))
	for _, id := range []string{"", "sync", "job:1", "job 1", "job\t1", "job\n1", "job\u00a01", "job\u007f1"} {
		assert.False(t, isValidPublicJobID(id), "id %q must be rejected", id)
	}
}

// TestRedisAsyncJobStore_PutWritesRecordAndIndexOnce asserts the Redis layout
// directly: one expiring record plus one index member per job, both carrying a
// TTL so an unpolled job cannot pin a pod forever.
func TestRedisAsyncJobStore_PutWritesRecordAndIndexOnce(t *testing.T) {
	ctx := context.Background()
	client, mr := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	record := testAsyncJobRecord(clock, asyncJobOwnerShared, "job-1", "backend-1")
	require.NoError(t, store.put(ctx, record))
	require.NoError(t, store.put(ctx, record))

	recordKey := asyncJobRecordKey(asyncJobOwnerShared, "job-1")
	indexKey := asyncJobIndexKey(asyncJobOwnerShared, asyncJobTypeVideo)

	members, err := client.ZRange(ctx, indexKey, 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{"job-1"}, members, "a retried write must not add a second index member")

	assert.Positive(t, mr.TTL(recordKey), "record must expire on its own")
	assert.Positive(t, mr.TTL(indexKey), "index must expire on its own")

	require.NoError(t, store.delete(ctx, asyncJobOwnerShared, "job-1"))
	assert.False(t, mr.Exists(recordKey))
	members, err = client.ZRange(ctx, indexKey, 0, -1).Result()
	require.NoError(t, err)
	assert.Empty(t, members)
}

// TestRedisAsyncJobStore_DeleteReapsIndexOfExpiredRecord covers the window
// where the record's own TTL fired but the index (which outlives no member in
// particular) still names the job: a DELETE must still clear the index member.
func TestRedisAsyncJobStore_DeleteReapsIndexOfExpiredRecord(t *testing.T) {
	ctx := context.Background()
	client, mr := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	record := testAsyncJobRecord(clock, asyncJobOwnerShared, "job-1", "backend-1")
	require.NoError(t, store.put(ctx, record))

	indexKey := asyncJobIndexKey(asyncJobOwnerShared, asyncJobTypeVideo)
	mr.Del(asyncJobRecordKey(asyncJobOwnerShared, "job-1"))

	require.NoError(t, store.delete(ctx, asyncJobOwnerShared, "job-1"))
	members, err := client.ZRange(ctx, indexKey, 0, -1).Result()
	require.NoError(t, err)
	assert.Empty(t, members, "index member must not survive its record")
}

// TestRedisAsyncJobStore_ListPrunesStaleIndexMembers covers the same window
// from the read side: List must not report (nor keep) a member whose record is
// already gone.
func TestRedisAsyncJobStore_ListPrunesStaleIndexMembers(t *testing.T) {
	ctx := context.Background()
	client, mr := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	require.NoError(t, store.put(ctx, testAsyncJobRecord(clock, asyncJobOwnerShared, "job-1", "backend-1")))
	require.NoError(t, store.put(ctx, testAsyncJobRecord(clock, asyncJobOwnerShared, "job-2", "backend-2")))
	mr.Del(asyncJobRecordKey(asyncJobOwnerShared, "job-2"))

	listed, err := store.list(ctx, asyncJobOwnerShared, asyncJobTypeVideo)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "job-1", listed[0].PublicJobID)

	members, err := client.ZRange(ctx, asyncJobIndexKey(asyncJobOwnerShared, asyncJobTypeVideo), 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{"job-1"}, members)
}

func TestRedisAsyncJobStore_ListPageUsesBoundedCursorRange(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	for _, id := range []string{"job-1", "job-2", "job-3"} {
		require.NoError(t, store.put(ctx, testAsyncJobRecord(clock, asyncJobOwnerShared, id, "backend-"+id)))
		clock.advance(time.Microsecond)
	}

	first, err := store.listPage(ctx, asyncJobOwnerShared, asyncJobTypeVideo, AsyncJobListOptions{Limit: 2, Order: "desc"})
	require.NoError(t, err)
	require.Len(t, first.Records, 2)
	assert.True(t, first.HasMore)
	assert.Equal(t, "job-3", first.Records[0].PublicJobID)
	assert.Equal(t, "job-2", first.Records[1].PublicJobID)

	second, err := store.listPage(ctx, asyncJobOwnerShared, asyncJobTypeVideo, AsyncJobListOptions{After: "job-2", Limit: 2, Order: "desc"})
	require.NoError(t, err)
	require.Len(t, second.Records, 1)
	assert.Equal(t, "job-1", second.Records[0].PublicJobID)
	assert.False(t, second.HasMore)
}

func TestAsyncJobStore_ListPageRejectsDeletedCursor(t *testing.T) {
	for _, fixture := range asyncJobStoreFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			require.NoError(t, fixture.store.put(ctx, testAsyncJobRecord(fixture.clock, asyncJobOwnerShared, "job-1", "backend-1")))
			fixture.advance(time.Microsecond)
			require.NoError(t, fixture.store.put(ctx, testAsyncJobRecord(fixture.clock, asyncJobOwnerShared, "job-cursor", "backend-cursor")))

			require.NoError(t, fixture.store.delete(ctx, asyncJobOwnerShared, "job-cursor"))
			pageStore, ok := fixture.store.(asyncJobPageStore)
			require.True(t, ok)
			_, err := pageStore.listPage(ctx, asyncJobOwnerShared, asyncJobTypeVideo, AsyncJobListOptions{
				After: "job-cursor",
				Limit: 1,
				Order: "desc",
			})
			assert.ErrorIs(t, err, errAsyncJobNotFound,
				"a deleted cursor has lost its sort position; callers must restart from the first page")
		})
	}
}

// asyncJobConcurrentInsertHook inserts a newer record immediately before the
// command that reads the page. With the old ZREVRANK + ZREVRANGE sequence this
// happened between those two commands and shifted the cursor's rank. The list
// script receives the insertion before it starts and then performs both steps
// atomically on Redis.
type asyncJobConcurrentInsertHook struct {
	once   sync.Once
	insert func() error
	err    error
}

func (h *asyncJobConcurrentInsertHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *asyncJobConcurrentInsertHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "evalsha" || cmd.Name() == "zrevrange" {
			h.once.Do(func() { h.err = h.insert() })
			if h.err != nil {
				return h.err
			}
		}
		return next(ctx, cmd)
	}
}

func (h *asyncJobConcurrentInsertHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestRedisAsyncJobStore_ListPageCursorIsStableAcrossConcurrentInsert(t *testing.T) {
	ctx := context.Background()
	client, mr := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	for _, id := range []string{"job-1", "job-2", "job-3", "job-4"} {
		require.NoError(t, store.put(ctx, testAsyncJobRecord(clock, asyncJobOwnerShared, id, "backend-"+id)))
		clock.advance(time.Microsecond)
	}
	otherClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = otherClient.Close() })
	otherStore := newRedisAsyncJobStore(otherClient)
	otherStore.now = clock.Now
	hook := &asyncJobConcurrentInsertHook{insert: func() error {
		return otherStore.put(ctx, testAsyncJobRecord(clock, asyncJobOwnerShared, "job-5", "backend-job-5"))
	}}
	client.AddHook(hook)

	page, err := store.listPage(ctx, asyncJobOwnerShared, asyncJobTypeVideo, AsyncJobListOptions{After: "job-3", Limit: 2, Order: "desc"})
	require.NoError(t, err)
	require.NoError(t, hook.err)
	require.Len(t, page.Records, 2)
	assert.Equal(t, "job-2", page.Records[0].PublicJobID)
	assert.Equal(t, "job-1", page.Records[1].PublicJobID)
	assert.False(t, page.HasMore)
}

func TestRedisAsyncJobStore_ListPageSkipsStaleMembersAndFillsPage(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	for _, id := range []string{"job-1", "job-2", "job-3"} {
		require.NoError(t, store.put(ctx, testAsyncJobRecord(clock, asyncJobOwnerShared, id, "backend-"+id)))
		clock.advance(time.Microsecond)
	}
	indexKey := asyncJobIndexKey(asyncJobOwnerShared, asyncJobTypeVideo)
	require.NoError(t, client.ZAdd(ctx, indexKey,
		redis.Z{Score: float64(clock.Now().UnixMicro()), Member: "job-stale-1"},
		redis.Z{Score: float64(clock.Now().Add(time.Microsecond).UnixMicro()), Member: "job-stale-2"}).Err())

	page, err := store.listPage(ctx, asyncJobOwnerShared, asyncJobTypeVideo, AsyncJobListOptions{Limit: 2, Order: "desc"})
	require.NoError(t, err)
	require.Len(t, page.Records, 2)
	assert.Equal(t, "job-3", page.Records[0].PublicJobID)
	assert.Equal(t, "job-2", page.Records[1].PublicJobID)
	assert.True(t, page.HasMore, "job-1 remains after the full page")

	members, err := client.ZRange(ctx, indexKey, 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{"job-1", "job-2", "job-3"}, members)
}

func TestRedisAsyncJobStore_ListPageScanBudgetIsRetryableAndMakesProgress(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	require.NoError(t, store.put(ctx, testAsyncJobRecord(clock, asyncJobOwnerShared, "job-live", "backend-live")))
	indexKey := asyncJobIndexKey(asyncJobOwnerShared, asyncJobTypeVideo)
	stale := make([]redis.Z, 0, asyncJobListMaxScan+1)
	for i := 0; i < asyncJobListMaxScan+1; i++ {
		stale = append(stale, redis.Z{Score: float64(clock.Now().Add(time.Duration(i+1) * time.Microsecond).UnixMicro()), Member: fmt.Sprintf("job-stale-%04d", i)})
	}
	require.NoError(t, client.ZAdd(ctx, indexKey, stale...).Err())

	_, err := store.listPage(ctx, asyncJobOwnerShared, asyncJobTypeVideo, AsyncJobListOptions{Limit: 1, Order: "desc"})
	assert.ErrorIs(t, err, errAsyncJobStoreUnavailable, "an inconclusive bounded scan must be retryable")

	page, err := store.listPage(ctx, asyncJobOwnerShared, asyncJobTypeVideo, AsyncJobListOptions{Limit: 1, Order: "desc"})
	require.NoError(t, err, "the first bounded scan must retain its stale-member cleanup")
	require.Len(t, page.Records, 1)
	assert.Equal(t, "job-live", page.Records[0].PublicJobID)
	assert.False(t, page.HasMore)
}

// TestRedisAsyncJobStore_TransientFailureIsRetryable checks the end-to-end
// classification: an unreachable Redis is a transient failure, so callers get
// errAsyncJobStoreUnavailable (a retryable 503) rather than a bare error that
// would be reported as a permanent failure.
func TestRedisAsyncJobStore_TransientFailureIsRetryable(t *testing.T) {
	ctx := context.Background()
	client, mr := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now
	mr.Close()

	record := testAsyncJobRecord(clock, asyncJobOwnerShared, "job-1", "backend-1")
	assert.ErrorIs(t, store.put(ctx, record), errAsyncJobStoreUnavailable)

	_, err := store.get(ctx, asyncJobOwnerShared, "job-1")
	assert.ErrorIs(t, err, errAsyncJobStoreUnavailable)

	_, err = store.list(ctx, asyncJobOwnerShared, asyncJobTypeVideo)
	assert.ErrorIs(t, err, errAsyncJobStoreUnavailable)

	assert.ErrorIs(t, store.delete(ctx, asyncJobOwnerShared, "job-1"), errAsyncJobStoreUnavailable)
}

// fakeRedisServerError stands in for a server-side reply error: redis.Error is
// an interface and its only concrete implementation lives in an internal
// package, so tests build their own.
type fakeRedisServerError string

func (e fakeRedisServerError) Error() string { return string(e) }

func (e fakeRedisServerError) RedisError() {}

func TestIsTransientRedisError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"redis nil reply", redis.Nil, false},
		{"loading", fakeRedisServerError("LOADING Redis is loading the dataset in memory"), true},
		{"tryagain", fakeRedisServerError("TRYAGAIN Multiple keys request during rehashing of slot"), true},
		{"clusterdown", fakeRedisServerError("CLUSTERDOWN The cluster is down"), true},
		{"masterdown", fakeRedisServerError("MASTERDOWN Link with MASTER is down"), true},
		{"readonly replica after failover", fakeRedisServerError("READONLY You can't write against a read only replica."), true},
		{"auth failure is permanent", fakeRedisServerError("NOAUTH Authentication required."), false},
		{"wrong type is permanent", fakeRedisServerError("WRONGTYPE Operation against a key holding the wrong kind of value"), false},
		{"script error is permanent", fakeRedisServerError("ERR Error compiling script"), false},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"serialization failure is permanent", errAsyncJobInvalidRecord, false},
		{"wrapped transient", fmt.Errorf("put: %w", io.EOF), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isTransientRedisError(tt.err))
		})
	}
}

func TestRetryAsyncJobStoreOp_RetriesOnlyTransientFailures(t *testing.T) {
	ctx := context.Background()
	policy := asyncJobRetryPolicy{maxAttempts: asyncJobStoreMaxAttempts, baseBackoff: time.Microsecond, deadline: time.Second}

	t.Run("succeeds without retrying", func(t *testing.T) {
		attempts := 0
		err := retryAsyncJobStoreOp(ctx, policy, "put", func(context.Context) error {
			attempts++
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 1, attempts)
	})

	t.Run("recovers on a later attempt", func(t *testing.T) {
		attempts := 0
		err := retryAsyncJobStoreOp(ctx, policy, "put", func(context.Context) error {
			attempts++
			if attempts < 2 {
				return io.EOF
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 2, attempts)
	})

	t.Run("gives up after the attempt budget", func(t *testing.T) {
		attempts := 0
		err := retryAsyncJobStoreOp(ctx, policy, "put", func(context.Context) error {
			attempts++
			return io.EOF
		})
		assert.ErrorIs(t, err, errAsyncJobStoreUnavailable)
		assert.Equal(t, asyncJobStoreMaxAttempts, attempts, "the attempt budget is total attempts, not retries on top of the first")
	})

	t.Run("does not retry a permanent failure", func(t *testing.T) {
		attempts := 0
		permanent := fakeRedisServerError("WRONGTYPE Operation against a key holding the wrong kind of value")
		err := retryAsyncJobStoreOp(ctx, policy, "put", func(context.Context) error {
			attempts++
			return permanent
		})
		assert.ErrorIs(t, err, permanent)
		assert.NotErrorIs(t, err, errAsyncJobStoreUnavailable)
		assert.Equal(t, 1, attempts)
	})

	t.Run("does not retry a not-found result", func(t *testing.T) {
		attempts := 0
		err := retryAsyncJobStoreOp(ctx, policy, "get", func(context.Context) error {
			attempts++
			return errAsyncJobNotFound
		})
		assert.ErrorIs(t, err, errAsyncJobNotFound)
		assert.Equal(t, 1, attempts)
	})
}

// TestRetryAsyncJobStoreOp_BoundedByDeadline pins the total wall-clock budget:
// the ext_proc response deadline, not the attempt count, is the real limit.
func TestRetryAsyncJobStoreOp_BoundedByDeadline(t *testing.T) {
	policy := asyncJobRetryPolicy{maxAttempts: asyncJobStoreMaxAttempts, baseBackoff: 10 * time.Millisecond, deadline: 5 * time.Millisecond}

	attempts := 0
	started := time.Now()
	err := retryAsyncJobStoreOp(context.Background(), policy, "put", func(context.Context) error {
		attempts++
		return io.EOF
	})
	assert.ErrorIs(t, err, errAsyncJobStoreUnavailable)
	assert.Less(t, attempts, asyncJobStoreMaxAttempts, "a deadline shorter than the backoff must cut the attempts short")
	assert.Less(t, time.Since(started), time.Second)
}

func TestRetryAsyncJobStoreOp_StopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	err := retryAsyncJobStoreOp(ctx, defaultAsyncJobRetryPolicy(), "put", func(context.Context) error {
		attempts++
		return io.EOF
	})
	assert.ErrorIs(t, err, errAsyncJobStoreUnavailable)
	assert.Equal(t, 1, attempts, "a cancelled request must not keep hammering Redis")
}

func TestDefaultAsyncJobRetryPolicy(t *testing.T) {
	policy := defaultAsyncJobRetryPolicy()
	assert.Equal(t, 3, policy.maxAttempts)
	assert.Positive(t, policy.baseBackoff)
	assert.Equal(t, time.Second, policy.deadline)
}

// asyncJobRegistryFixture wires a registry over the in-memory store and a
// MockCache pod resolver, with the clock under the test's control.
type asyncJobRegistryFixture struct {
	registry *memoryAsyncJobRegistry
	store    asyncJobStore
	cache    *MockCache
	clock    *fakeClock
}

func newTestAsyncJobRegistryWithStore(store asyncJobStore, pods podResolver) *memoryAsyncJobRegistry {
	return &memoryAsyncJobRegistry{asyncJobRegistryCore: newAsyncJobRegistryCore(store, pods)}
}

func listTestAsyncJobs(ctx context.Context, registry AsyncJobRegistry, owner, jobType string) ([]AsyncJobRecord, error) {
	page, err := registry.List(ctx, owner, jobType, AsyncJobListOptions{Limit: maxAsyncJobListLimit, Order: "desc"})
	return page.Records, err
}

func newAsyncJobRegistryFixture(t *testing.T) *asyncJobRegistryFixture {
	t.Helper()
	clock := newFakeClock()
	store := newInMemoryAsyncJobStore()
	store.now = clock.Now
	mockCache := new(MockCache)
	registry := newTestAsyncJobRegistryWithStore(store, mockCache)
	registry.now = clock.Now
	return &asyncJobRegistryFixture{registry: registry, store: store, cache: mockCache, clock: clock}
}

func TestAsyncJobRegistry_RegisterGeneratesOpaquePublicID(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")

	record, err := fixture.registry.Register(context.Background(), AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        asyncJobOwnerShared,
		Model:        "wan2.1",
		BackendJobID: "video_gen_abc123",
		Pod:          pod,
	})
	require.NoError(t, err)

	assert.NotEqual(t, "video_gen_abc123", record.PublicJobID, "the public id must never be the backend job id")
	assert.NotContains(t, record.PublicJobID, "video_gen_abc123")
	assert.True(t, strings.HasPrefix(record.PublicJobID, asyncJobPublicIDPrefix))
	assert.Equal(t, asyncJobTargetKindPod, record.RoutingTarget.Kind)
	assert.Equal(t, "pod-a", record.RoutingTarget.Pod.Name)
	assert.Equal(t, "ns-a", record.RoutingTarget.Pod.Namespace)
	assert.Equal(t, string(pod.UID), record.RoutingTarget.Pod.UID)
	assert.True(t, record.CreatedAt.Equal(fixture.clock.Now()))

	// The record is durable before Register returns: no gateway-replica cache
	// stands between the caller and the store.
	stored, err := fixture.store.get(context.Background(), asyncJobOwnerShared, record.PublicJobID)
	require.NoError(t, err)
	assert.Equal(t, "video_gen_abc123", stored.BackendJobID)
}

func TestAsyncJobRegistry_RegisterAppliesBackendExpiry(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")
	backendExpiry := fixture.clock.Now().Add(30 * time.Minute)

	record, err := fixture.registry.Register(context.Background(), AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        asyncJobOwnerShared,
		Model:        "wan2.1",
		BackendJobID: "backend-1",
		Pod:          pod,
		ExpiresAt:    backendExpiry,
	})
	require.NoError(t, err)
	assert.True(t, backendExpiry.Equal(record.ExpiresAt), "the backend's own expires_at wins when it reports one")
}

func TestAsyncJobRegistry_RegisterDefaultsExpiry(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")

	for _, tt := range []struct {
		name      string
		expiresAt time.Time
	}{
		{"absent", time.Time{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			record, err := fixture.registry.Register(context.Background(), AsyncJobRegistration{
				JobType:      asyncJobTypeVideo,
				Owner:        asyncJobOwnerShared,
				Model:        "wan2.1",
				BackendJobID: "backend-1",
				Pod:          pod,
				ExpiresAt:    tt.expiresAt,
			})
			require.NoError(t, err)
			assert.True(t, fixture.clock.Now().Add(defaultAsyncJobTTL).Equal(record.ExpiresAt))
		})
	}
}

func TestAsyncJobRegistry_RegisterRejectsIncompleteInput(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")
	podWithoutUID := asyncJobReadyPod("pod-b", "ns-b", "10.0.0.6")
	podWithoutUID.UID = ""

	tests := []struct {
		name string
		reg  AsyncJobRegistration
	}{
		{"no pod", AsyncJobRegistration{JobType: asyncJobTypeVideo, Owner: asyncJobOwnerShared, BackendJobID: "backend-1"}},
		{"no backend id", AsyncJobRegistration{JobType: asyncJobTypeVideo, Owner: asyncJobOwnerShared, Pod: pod}},
		{"no job type", AsyncJobRegistration{Owner: asyncJobOwnerShared, BackendJobID: "backend-1", Pod: pod}},
		{"no owner", AsyncJobRegistration{JobType: asyncJobTypeVideo, BackendJobID: "backend-1", Pod: pod}},
		{"pod without uid", AsyncJobRegistration{JobType: asyncJobTypeVideo, Owner: asyncJobOwnerShared, BackendJobID: "backend-1", Pod: podWithoutUID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fixture.registry.Register(context.Background(), tt.reg)
			assert.ErrorIs(t, err, errAsyncJobInvalidRecord)
		})
	}
}

func TestAsyncJobRegistry_GetResolvesRecordedPod(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	ctx := context.Background()
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")

	record, err := fixture.registry.Register(ctx, AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        asyncJobOwnerShared,
		Model:        "wan2.1",
		BackendJobID: "backend-1",
		Pod:          pod,
	})
	require.NoError(t, err)

	fixture.cache.On("GetPod", "pod-a", "ns-a").Return(pod, nil)

	got, resolved, err := fixture.registry.Get(ctx, asyncJobOwnerShared, record.PublicJobID)
	require.NoError(t, err)
	assert.Equal(t, "backend-1", got.BackendJobID)
	assert.Same(t, pod, resolved)
	fixture.cache.AssertExpectations(t)
}

// TestAsyncJobRegistry_GetRejectsReplacedPodUID is the reason the record stores
// a UID at all: a pod recreated under the same name is a different pod, and the
// job's output does not exist on its disk.
func TestAsyncJobRegistry_GetRejectsReplacedPodUID(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	ctx := context.Background()
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")

	record, err := fixture.registry.Register(ctx, AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        asyncJobOwnerShared,
		Model:        "wan2.1",
		BackendJobID: "backend-1",
		Pod:          pod,
	})
	require.NoError(t, err)

	replacement := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.9")
	replacement.UID = "uid-pod-a-recreated"
	fixture.cache.On("GetPod", "pod-a", "ns-a").Return(replacement, nil)

	_, _, err = fixture.registry.Get(ctx, asyncJobOwnerShared, record.PublicJobID)
	assert.ErrorIs(t, err, errAsyncJobNotFound)

	_, err = fixture.store.get(ctx, asyncJobOwnerShared, record.PublicJobID)
	assert.ErrorIs(t, err, errAsyncJobNotFound, "a record pointing at a replaced pod is terminal and must be dropped")
	fixture.cache.AssertExpectations(t)
}

func TestAsyncJobRegistry_GetDropsRecordWhenPodIsGone(t *testing.T) {
	terminating := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")
	deletedAt := metav1.Now()
	terminating.DeletionTimestamp = &deletedAt

	tests := []struct {
		name string
		pod  *v1.Pod
		err  error
	}{
		{"not in the informer cache", nil, errors.New("pod not found")},
		{"terminating", terminating, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newAsyncJobRegistryFixture(t)
			ctx := context.Background()
			pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")

			record, err := fixture.registry.Register(ctx, AsyncJobRegistration{
				JobType:      asyncJobTypeVideo,
				Owner:        asyncJobOwnerShared,
				Model:        "wan2.1",
				BackendJobID: "backend-1",
				Pod:          pod,
			})
			require.NoError(t, err)

			fixture.cache.On("GetPod", "pod-a", "ns-a").Return(tt.pod, tt.err)

			_, _, err = fixture.registry.Get(ctx, asyncJobOwnerShared, record.PublicJobID)
			assert.ErrorIs(t, err, errAsyncJobNotFound)

			_, err = fixture.store.get(ctx, asyncJobOwnerShared, record.PublicJobID)
			assert.ErrorIs(t, err, errAsyncJobNotFound)
			fixture.cache.AssertExpectations(t)
		})
	}
}

func TestAsyncJobRegistry_GetKeepsRecordWhenPodIsOnlyTransientlyUnavailable(t *testing.T) {
	notReady := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")
	notReady.Status.Conditions = nil
	noAddress := asyncJobReadyPod("pod-a", "ns-a", "")

	tests := []struct {
		name string
		pod  *v1.Pod
	}{
		{"not ready", notReady},
		{"no routable address yet", noAddress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newAsyncJobRegistryFixture(t)
			ctx := context.Background()
			pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")

			record, err := fixture.registry.Register(ctx, AsyncJobRegistration{
				JobType:      asyncJobTypeVideo,
				Owner:        asyncJobOwnerShared,
				Model:        "wan2.1",
				BackendJobID: "backend-1",
				Pod:          pod,
			})
			require.NoError(t, err)

			fixture.cache.On("GetPod", "pod-a", "ns-a").Return(tt.pod, nil)

			_, _, err = fixture.registry.Get(ctx, asyncJobOwnerShared, record.PublicJobID)
			assert.ErrorIs(t, err, errAsyncJobTargetUnavailable)

			_, err = fixture.store.get(ctx, asyncJobOwnerShared, record.PublicJobID)
			assert.NoError(t, err, "a transiently unavailable pod must not cost the client its job record")
			fixture.cache.AssertExpectations(t)
		})
	}
}

func TestAsyncJobRegistry_GetEnforcesOwnerScope(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	ctx := context.Background()
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")
	alice := asyncJobOwnerFromUserName("alice")

	record, err := fixture.registry.Register(ctx, AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        alice,
		Model:        "wan2.1",
		BackendJobID: "backend-1",
		Pod:          pod,
	})
	require.NoError(t, err)

	_, _, err = fixture.registry.Get(ctx, asyncJobOwnerFromUserName("bob"), record.PublicJobID)
	assert.ErrorIs(t, err, errAsyncJobNotFound)

	_, _, err = fixture.registry.Get(ctx, asyncJobOwnerShared, record.PublicJobID)
	assert.ErrorIs(t, err, errAsyncJobNotFound, "scope:shared must not act as a wildcard over user scopes")

	// A rejected read must not have touched the pod cache, and must not have
	// dropped the owner's record.
	fixture.cache.AssertNotCalled(t, "GetPod", "pod-a", "ns-a")
	_, err = fixture.store.get(ctx, alice, record.PublicJobID)
	assert.NoError(t, err)
}

func TestAsyncJobRegistry_GetExpiredRecordIsNotFound(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	ctx := context.Background()
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")

	record, err := fixture.registry.Register(ctx, AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        asyncJobOwnerShared,
		Model:        "wan2.1",
		BackendJobID: "backend-1",
		Pod:          pod,
		ExpiresAt:    fixture.clock.Now().Add(time.Minute),
	})
	require.NoError(t, err)

	fixture.clock.advance(2 * time.Minute)

	_, _, err = fixture.registry.Get(ctx, asyncJobOwnerShared, record.PublicJobID)
	assert.ErrorIs(t, err, errAsyncJobNotFound)

	listed, err := listTestAsyncJobs(ctx, fixture.registry, asyncJobOwnerShared, asyncJobTypeVideo)
	require.NoError(t, err)
	assert.Empty(t, listed)
}

func TestAsyncJobRegistry_ListIsScopedAndTyped(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	ctx := context.Background()
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")
	alice := asyncJobOwnerFromUserName("alice")

	for _, backendID := range []string{"backend-1", "backend-2"} {
		_, err := fixture.registry.Register(ctx, AsyncJobRegistration{
			JobType:      asyncJobTypeVideo,
			Owner:        alice,
			Model:        "wan2.1",
			BackendJobID: backendID,
			Pod:          pod,
		})
		require.NoError(t, err)
	}
	_, err := fixture.registry.Register(ctx, AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        asyncJobOwnerShared,
		Model:        "wan2.1",
		BackendJobID: "backend-shared",
		Pod:          pod,
	})
	require.NoError(t, err)

	listed, err := listTestAsyncJobs(ctx, fixture.registry, alice, asyncJobTypeVideo)
	require.NoError(t, err)
	assert.Len(t, listed, 2)

	listed, err = listTestAsyncJobs(ctx, fixture.registry, asyncJobOwnerShared, asyncJobTypeVideo)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "backend-shared", listed[0].BackendJobID)

	// Listing resolves nothing: it is a registry read, not a backend probe.
	fixture.cache.AssertNotCalled(t, "GetPod", "pod-a", "ns-a")
}

func TestAsyncJobRegistry_DeleteIsScopedAndIdempotent(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	ctx := context.Background()
	pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")
	alice := asyncJobOwnerFromUserName("alice")

	record, err := fixture.registry.Register(ctx, AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        alice,
		Model:        "wan2.1",
		BackendJobID: "backend-1",
		Pod:          pod,
	})
	require.NoError(t, err)

	require.NoError(t, fixture.registry.Delete(ctx, asyncJobOwnerFromUserName("bob"), record.PublicJobID))
	_, err = fixture.store.get(ctx, alice, record.PublicJobID)
	require.NoError(t, err, "another scope's delete must not reach this record")

	require.NoError(t, fixture.registry.Delete(ctx, alice, record.PublicJobID))
	_, err = fixture.store.get(ctx, alice, record.PublicJobID)
	assert.ErrorIs(t, err, errAsyncJobNotFound)

	assert.NoError(t, fixture.registry.Delete(ctx, alice, record.PublicJobID))
}

// asyncJobTypeTestAudio stands in for any future job type. The registry is
// generic: nothing about the record or the index layout may be specific to
// video, or adding a second job type would leak index members on delete.
const asyncJobTypeTestAudio = "audio"

// TestRedisAsyncJobStore_DeleteReapsIndexOfAnyJobType asserts the delete
// contract for a job type the code has no constant for. Delete has to find the
// record's index from data, not from a hard-coded list of known job types.
func TestRedisAsyncJobStore_DeleteReapsIndexOfAnyJobType(t *testing.T) {
	ctx := context.Background()
	client, mr := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	record := testAsyncJobRecord(clock, asyncJobOwnerShared, "job-audio", "backend-audio")
	record.JobType = asyncJobTypeTestAudio
	require.NoError(t, store.put(ctx, record))

	indexKey := asyncJobIndexKey(asyncJobOwnerShared, asyncJobTypeTestAudio)
	members, err := client.ZRange(ctx, indexKey, 0, -1).Result()
	require.NoError(t, err)
	require.Equal(t, []string{"job-audio"}, members)

	require.NoError(t, store.delete(ctx, asyncJobOwnerShared, "job-audio"))
	assert.False(t, mr.Exists(asyncJobRecordKey(asyncJobOwnerShared, "job-audio")))
	members, err = client.ZRange(ctx, indexKey, 0, -1).Result()
	require.NoError(t, err)
	assert.Empty(t, members, "a non-video job's index member must not survive its record")
}

// TestRedisAsyncJobStore_DeleteReapsExpiredRecordIndexOfAnyJobType is the same
// contract in the window where the record's own TTL fired first: its job type is
// no longer readable, so the index it belongs to must be discoverable without it.
func TestRedisAsyncJobStore_DeleteReapsExpiredRecordIndexOfAnyJobType(t *testing.T) {
	ctx := context.Background()
	client, mr := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	record := testAsyncJobRecord(clock, asyncJobOwnerShared, "job-audio", "backend-audio")
	record.JobType = asyncJobTypeTestAudio
	require.NoError(t, store.put(ctx, record))
	mr.Del(asyncJobRecordKey(asyncJobOwnerShared, "job-audio"))

	require.NoError(t, store.delete(ctx, asyncJobOwnerShared, "job-audio"))
	members, err := client.ZRange(ctx, asyncJobIndexKey(asyncJobOwnerShared, asyncJobTypeTestAudio), 0, -1).Result()
	require.NoError(t, err)
	assert.Empty(t, members, "an expired record's index member must still be reaped")
}

// TestRedisAsyncJobStore_DeleteLeavesOtherJobsIndexed guards the blast radius of
// the data-driven delete: clearing one job's index membership must not empty the
// index for the owner's other jobs.
func TestRedisAsyncJobStore_DeleteLeavesOtherJobsIndexed(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestAsyncJobRedisClient(t)
	clock := newFakeClock()
	store := newRedisAsyncJobStore(client)
	store.now = clock.Now

	require.NoError(t, store.put(ctx, testAsyncJobRecord(clock, asyncJobOwnerShared, "job-1", "backend-1")))
	audio := testAsyncJobRecord(clock, asyncJobOwnerShared, "job-2", "backend-2")
	audio.JobType = asyncJobTypeTestAudio
	require.NoError(t, store.put(ctx, audio))

	require.NoError(t, store.delete(ctx, asyncJobOwnerShared, "job-2"))

	video, err := store.list(ctx, asyncJobOwnerShared, asyncJobTypeVideo)
	require.NoError(t, err)
	require.Len(t, video, 1)
	assert.Equal(t, "job-1", video[0].PublicJobID)

	remaining, err := store.list(ctx, asyncJobOwnerShared, asyncJobTypeTestAudio)
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

// TestRetryAsyncJobStoreOp_BoundsAHungAttempt is the deadline that matters: an
// attempt that never answers must be cut off, not waited out. Envoy is timing
// the ext_proc exchange this runs inside, so the operation - not just the sleep
// between attempts - has to carry the deadline.
func TestRetryAsyncJobStoreOp_BoundsAHungAttempt(t *testing.T) {
	policy := asyncJobRetryPolicy{maxAttempts: asyncJobStoreMaxAttempts, baseBackoff: time.Millisecond, deadline: 80 * time.Millisecond}

	attempts := 0
	started := time.Now()
	err := retryAsyncJobStoreOp(context.Background(), policy, "put", func(opCtx context.Context) error {
		attempts++
		// A Redis call that never comes back on its own: only a deadline on the
		// context handed to it can end this.
		<-opCtx.Done()
		return opCtx.Err()
	})
	elapsed := time.Since(started)

	assert.ErrorIs(t, err, errAsyncJobStoreUnavailable, "a store that never answered is retryable for the client")
	assert.Equal(t, 1, attempts)
	assert.Less(t, elapsed, 2*policy.deadline, "the whole operation must be bounded by the policy deadline")
}

// TestRetryAsyncJobStoreOp_BoundedByCallerDeadline pins the other half of the
// bound: the ext_proc deadline wins whenever it is shorter than the policy's.
func TestRetryAsyncJobStoreOp_BoundedByCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := retryAsyncJobStoreOp(ctx, defaultAsyncJobRetryPolicy(), "get", func(opCtx context.Context) error {
		<-opCtx.Done()
		return opCtx.Err()
	})
	elapsed := time.Since(started)

	assert.ErrorIs(t, err, errAsyncJobStoreUnavailable)
	assert.Less(t, elapsed, asyncJobStoreRetryDeadline, "a caller deadline shorter than the policy's must cap the operation")
}

// failingDeleteAsyncJobStore fails every delete transiently, the way a Redis
// outage would once the bounded retries are exhausted.
type failingDeleteAsyncJobStore struct {
	asyncJobStore
	deletes int
}

func (s *failingDeleteAsyncJobStore) delete(context.Context, string, string) error {
	s.deletes++
	return fmt.Errorf("%w: delete: %v", errAsyncJobStoreUnavailable, io.EOF)
}

// TestAsyncJobRegistry_GetReportsFailedCleanupAsRetryable covers the case where
// the pod is confirmed gone but the record cannot be deleted. Answering 404 there
// would tell the client the job is gone while the record is still durable, so the
// next request pins the same dead pod again: the store failure has to surface as
// a retryable 503 instead.
func TestAsyncJobRegistry_GetReportsFailedCleanupAsRetryable(t *testing.T) {
	terminating := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")
	deletedAt := metav1.Now()
	terminating.DeletionTimestamp = &deletedAt

	replaced := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.9")
	replaced.UID = "uid-pod-a-recreated"

	tests := []struct {
		name string
		pod  *v1.Pod
		err  error
	}{
		{"not in the informer cache", nil, errors.New("pod not found")},
		{"terminating", terminating, nil},
		{"replaced under the same name", replaced, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newAsyncJobRegistryFixture(t)
			ctx := context.Background()
			pod := asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5")

			record, err := fixture.registry.Register(ctx, AsyncJobRegistration{
				JobType:      asyncJobTypeVideo,
				Owner:        asyncJobOwnerShared,
				Model:        "wan2.1",
				BackendJobID: "backend-1",
				Pod:          pod,
			})
			require.NoError(t, err)

			broken := &failingDeleteAsyncJobStore{asyncJobStore: fixture.store}
			fixture.registry.store = broken
			fixture.cache.On("GetPod", "pod-a", "ns-a").Return(tt.pod, tt.err)

			got, resolved, err := fixture.registry.Get(ctx, asyncJobOwnerShared, record.PublicJobID)
			assert.ErrorIs(t, err, errAsyncJobStoreUnavailable)
			assert.NotErrorIs(t, err, errAsyncJobNotFound, "a 404 must not be reported while the stale record is still durable")
			assert.Nil(t, resolved)
			assert.Equal(t, "wan2.1", got.Model, "the caller still needs the model to attribute the failure")
			assert.Positive(t, broken.deletes, "the mandatory cleanup must have been attempted")

			_, err = fixture.store.get(ctx, asyncJobOwnerShared, record.PublicJobID)
			assert.NoError(t, err, "the record is still there, which is exactly why the answer is retryable")
			fixture.cache.AssertExpectations(t)
		})
	}
}

// TestAsyncJobRegistry_RegisterRejectsElapsedBackendExpiry pins the half of the
// expiry rule that the default TTL must not paper over: when the backend reports
// an expires_at that has already passed, the job is dead on arrival. Extending it
// to the seven-day default would keep routing to a job whose output the backend
// has already dropped, so the registration is refused outright - before a public
// id is minted, so nothing can be handed to a client either.
func TestAsyncJobRegistry_RegisterRejectsElapsedBackendExpiry(t *testing.T) {
	fixture := newAsyncJobRegistryFixture(t)
	ctx := context.Background()

	for _, tt := range []struct {
		name      string
		expiresAt time.Time
	}{
		{"already in the past", fixture.clock.Now().Add(-time.Minute)},
		{"expiring exactly now", fixture.clock.Now()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			record, err := fixture.registry.Register(ctx, AsyncJobRegistration{
				JobType:      asyncJobTypeVideo,
				Owner:        asyncJobOwnerShared,
				Model:        "wan2.1",
				BackendJobID: "backend-1",
				Pod:          asyncJobReadyPod("pod-a", "ns-a", "10.0.0.5"),
				ExpiresAt:    tt.expiresAt,
			})
			assert.ErrorIs(t, err, errAsyncJobInvalidRecord)
			assert.Empty(t, record.PublicJobID, "no public id may be minted for a job that is already expired")

			records, listErr := listTestAsyncJobs(ctx, fixture.registry, asyncJobOwnerShared, asyncJobTypeVideo)
			require.NoError(t, listErr)
			assert.Empty(t, records, "an expired registration must leave nothing behind")
		})
	}
}

// countingRedisServer is a Redis server that answers every command with one
// transient error and counts what it was asked. It exists because a wire attempt
// is the only thing that can be counted here: go-redis retries inside its own
// process() call, so a hook or a callback counter sees one operation no matter
// how many times the command actually went out.
type countingRedisServer struct {
	listener net.Listener
	reply    string
	blockGET chan struct{}

	mu     sync.Mutex
	counts map[string]int
}

func newCountingRedisServer(t *testing.T, reply string) *countingRedisServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := &countingRedisServer{listener: listener, reply: reply, counts: map[string]int{}}
	go server.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

func newBlockingRedisServer(t *testing.T) *countingRedisServer {
	t.Helper()
	server := newCountingRedisServer(t, "OK")
	server.blockGET = make(chan struct{})
	t.Cleanup(func() { close(server.blockGET) })
	return server
}

func (s *countingRedisServer) addr() string { return s.listener.Addr().String() }

func (s *countingRedisServer) count(command string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[strings.ToUpper(command)]
}

func (s *countingRedisServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *countingRedisServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	for {
		command, err := readRESPCommand(reader)
		if err != nil {
			return
		}
		if len(command) == 0 {
			continue
		}
		name := strings.ToUpper(command[0])
		s.mu.Lock()
		s.counts[name]++
		s.mu.Unlock()
		if name == "GET" && s.blockGET != nil {
			<-s.blockGET
			return
		}
		// HELLO is the client's handshake, not a command this test is about. It is
		// refused the way a RESP2-only server does, which sends go-redis down its
		// plain AUTH/SELECT path instead of failing the connection.
		reply := s.reply
		if name == "HELLO" {
			reply = "ERR unknown command 'HELLO'"
		}
		if _, err := conn.Write([]byte("-" + reply + "\r\n")); err != nil {
			return
		}
	}
}

// readRESPCommand reads one RESP array of bulk strings, which is how a client
// sends a command. Inline commands are not supported: go-redis never sends them.
func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	header = strings.TrimRight(header, "\r\n")
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("unsupported RESP frame %q", header)
	}
	count, err := strconv.Atoi(strings.TrimPrefix(header, "*"))
	if err != nil {
		return nil, err
	}

	command := make([]string, 0, count)
	for i := 0; i < count; i++ {
		sizeLine, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimRight(strings.TrimPrefix(sizeLine, "$"), "\r\n"))
		if err != nil {
			return nil, err
		}
		payload := make([]byte, size+2) // +2 for the trailing CRLF
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, err
		}
		command = append(command, string(payload[:size]))
	}
	return command, nil
}

// TestAsyncJobStoreClient_DisablesGoRedisInternalRetries proves the three-attempt
// budget is the whole budget. go-redis retries three times on its own by default,
// which would turn this package's three attempts into twelve round trips and
// leave the one-second deadline to do the bounding on its own.
func TestAsyncJobStoreClient_DisablesGoRedisInternalRetries(t *testing.T) {
	server := newCountingRedisServer(t, "LOADING Redis is loading the dataset in memory")

	client := redis.NewClient(&redis.Options{Addr: server.addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := newRedisAsyncJobStore(asyncJobStoreClient(client))
	_, err := store.get(context.Background(), asyncJobOwnerShared, "job-1")
	assert.ErrorIs(t, err, errAsyncJobStoreUnavailable)

	assert.Equal(t, asyncJobStoreMaxAttempts, server.count("GET"),
		"a transient failure must cost exactly the policy's attempts, retries included")
}

func TestAsyncJobStoreClient_BoundsBlockedNetworkReadByContext(t *testing.T) {
	server := newBlockingRedisServer(t)
	client := redis.NewClient(&redis.Options{Addr: server.addr()})
	t.Cleanup(func() { _ = client.Close() })

	derived := asyncJobStoreClient(client)
	t.Cleanup(func() { _ = derived.Close() })
	store := newRedisAsyncJobStore(derived)
	store.retry = asyncJobRetryPolicy{maxAttempts: 1, baseBackoff: time.Millisecond, deadline: 80 * time.Millisecond}

	started := time.Now()
	_, err := store.get(context.Background(), asyncJobOwnerShared, "job-1")
	elapsed := time.Since(started)

	assert.ErrorIs(t, err, errAsyncJobStoreUnavailable)
	assert.Less(t, elapsed, 2*store.retry.deadline, "the operation deadline must interrupt a blocked Redis read")
	assert.Equal(t, 1, server.count("GET"))
}

// TestAsyncJobStoreClient_KeepsConnectionSettings keeps the derived client from
// quietly dropping the address or the credentials of the client it came from.
func TestAsyncJobStoreClient_KeepsConnectionSettings(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "10.0.0.9:6379", Password: "s3cret", DB: 3})
	t.Cleanup(func() { _ = client.Close() })

	derived := asyncJobStoreClient(client)
	t.Cleanup(func() { _ = derived.Close() })

	assert.Equal(t, "10.0.0.9:6379", derived.Options().Addr)
	assert.Equal(t, "s3cret", derived.Options().Password)
	assert.Equal(t, 3, derived.Options().DB)
	assert.Equal(t, 0, derived.Options().MaxRetries, "go-redis normalizes a disabled retry loop to zero retries")
	assert.True(t, derived.Options().ContextTimeoutEnabled)
}
