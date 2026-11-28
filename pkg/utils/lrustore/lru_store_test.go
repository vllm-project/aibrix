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

package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLRUStore_ZeroInterval(t *testing.T) {
	store := NewLRUStore[string, string](2, time.Second, 0, DefaultGetCurrentTime)
	t.Cleanup(func() { store.Close() })

	store.Put("key", "value")
	if val, ok := store.Get("key"); !ok || val != "value" {
		t.Fatalf("expected value, got %v ok=%v", val, ok)
	}
}

func TestLRUStore_CloseIdempotent(t *testing.T) {
	store := NewLRUStore[string, string](2, time.Second, time.Hour, DefaultGetCurrentTime)
	store.Close()
	store.Close()
}

func TestLRUStore_PutAndGet(t *testing.T) {
	store := NewLRUStore[string, string](2, 5*time.Second, 1*time.Second, DefaultGetCurrentTime)
	t.Cleanup(func() { store.Close() })

	// Test adding and retrieving items
	store.Put("key1", "value1")
	store.Put("key2", "value2")

	if val, ok := store.Get("key1"); !ok || val != "value1" {
		t.Errorf("expected value1, got %v", val)
	}

	if val, ok := store.Get("key2"); !ok || val != "value2" {
		t.Errorf("expected value2, got %v", val)
	}

	store.Put("key3", "value3")
	if _, ok := store.Get("key1"); ok {
		t.Errorf("expected key1 to be evicted")
	}

	if val, ok := store.Get("key3"); !ok || val != "value3" {
		t.Errorf("expected value3, got %v", val)
	}
}

func TestLRUStore_TTL(t *testing.T) {
	// Use a fake clock so the test doesn't depend on wall time.
	var unixNano atomic.Int64
	unixNano.Store(time.Now().UnixNano())
	fakeClock := func() time.Time { return time.Unix(0, unixNano.Load()) }

	ttl := 2 * time.Second
	store := NewLRUStore[string, string](2, ttl, time.Hour, fakeClock)
	t.Cleanup(func() { store.Close() })

	store.Put("key1", "value1")

	// Advance fake time past the TTL and trigger eviction manually.
	unixNano.Add(int64(ttl + time.Millisecond))
	if evicted := store.Evict(fakeClock()); evicted != 1 {
		t.Fatalf("expected 1 evicted entry, got %d", evicted)
	}

	if _, ok := store.Get("key1"); ok {
		t.Error("expected key1 to be expired after TTL")
	}
}

func TestLRUStore_UpdateExistingKey(t *testing.T) {
	store := NewLRUStore[string, string](2, 5*time.Second, 1*time.Second, DefaultGetCurrentTime)
	t.Cleanup(func() { store.Close() })

	// Test updating an existing key
	store.Put("key1", "value1")
	store.Put("key1", "value2")

	if val, ok := store.Get("key1"); !ok || val != "value2" {
		t.Errorf("expected value2, got %v", val)
	}
}

func TestLRUStore_EvictEmptyPartialFull(t *testing.T) {
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	now := base
	ttl := 10 * time.Second
	store := NewLRUStore[string, int](16, ttl, 0, func() time.Time { return now })
	t.Cleanup(func() { store.Close() })

	// Empty store.
	if evicted := store.Evict(now); evicted != 0 {
		t.Fatalf("expected 0 evicted on empty store, got %d", evicted)
	}

	// Insert 4 old keys at t=0.
	store.Put("refreshed", 1)
	store.Put("old-1", 2)
	store.Put("old-2", 3)
	store.Put("old-3", 4)

	// Refresh the oldest key and add 2 fresh keys at t=8s.
	now = base.Add(8 * time.Second)
	store.Put("refreshed", 10)
	store.Put("fresh-1", 5)
	store.Put("fresh-2", 6)

	// At t=11s: old-1/2/3 are expired (11s > 10s); refreshed & fresh-1/2 are still valid (3s <= 10s).
	if evicted := store.Evict(base.Add(11 * time.Second)); evicted != 3 {
		t.Fatalf("expected 3 evicted, got %d", evicted)
	}
	if store.Len() != 3 {
		t.Fatalf("expected len 3, got %d", store.Len())
	}
	for _, k := range []string{"old-1", "old-2", "old-3"} {
		if _, ok := store.Get(k); ok {
			t.Errorf("expected %s to be evicted", k)
		}
	}
	if v, ok := store.Get("refreshed"); !ok || v != 10 {
		t.Errorf("expected refreshed=10, got %v ok=%v", v, ok)
	}
	for _, k := range []string{"fresh-1", "fresh-2"} {
		if _, ok := store.Get(k); !ok {
			t.Errorf("expected %s to remain", k)
		}
	}

	// At t=20s: all remaining entries expire.
	if evicted := store.Evict(base.Add(20 * time.Second)); evicted != 3 {
		t.Fatalf("expected remaining 3 evicted, got %d", evicted)
	}
	if store.Len() != 0 {
		t.Fatalf("expected empty store, got len %d", store.Len())
	}
	if evicted := store.Evict(base.Add(25 * time.Second)); evicted != 0 {
		t.Fatalf("expected 0 evicted after full drain, got %d", evicted)
	}
}

func TestLRUStore_EvictStrictGreaterThanAndZeroTTL(t *testing.T) {
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	t.Run("positive_ttl_exact_boundary", func(t *testing.T) {
		ttl := 5 * time.Second
		store := NewLRUStore[string, int](4, ttl, 0, func() time.Time { return base })
		t.Cleanup(func() { store.Close() })
		store.Put("k1", 1)

		// now.Sub(lastAccessTime) == ttl must NOT expire (strict > check).
		if evicted := store.Evict(base.Add(ttl)); evicted != 0 {
			t.Fatalf("expected 0 evicted at exact TTL boundary, got %d", evicted)
		}
		if _, ok := store.Get("k1"); !ok {
			t.Fatal("expected k1 to survive exact TTL boundary")
		}

		// ttl + 1ns MUST expire.
		if evicted := store.Evict(base.Add(ttl + time.Nanosecond)); evicted != 1 {
			t.Fatalf("expected 1 evicted past TTL boundary, got %d", evicted)
		}
		if _, ok := store.Get("k1"); ok {
			t.Fatal("expected k1 to expire past TTL boundary")
		}
	})

	t.Run("zero_ttl_exact_boundary", func(t *testing.T) {
		store := NewLRUStore[string, int](4, 0, 0, func() time.Time { return base })
		t.Cleanup(func() { store.Close() })
		store.Put("k1", 1)

		// 0 > 0 is false: entry inserted at base must not expire at base.
		if evicted := store.Evict(base); evicted != 0 {
			t.Fatalf("expected 0 evicted at same timestamp with ttl=0, got %d", evicted)
		}
		if _, ok := store.Get("k1"); !ok {
			t.Fatal("expected k1 to survive at same timestamp with ttl=0")
		}

		// 1ns > 0 is true: entry expires once time advances.
		if evicted := store.Evict(base.Add(time.Nanosecond)); evicted != 1 {
			t.Fatalf("expected 1 evicted after 1ns with ttl=0, got %d", evicted)
		}
		if _, ok := store.Get("k1"); ok {
			t.Fatal("expected k1 to expire after 1ns with ttl=0")
		}
	})
}

func TestLRUStore_EvictNonMonotonicClockFallback(t *testing.T) {
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	now := base.Add(100 * time.Second)
	ttl := 30 * time.Second
	store := NewLRUStore[string, int](1024, ttl, 0, func() time.Time { return now })
	t.Cleanup(func() { store.Close() })

	// Insert a newer entry first (at tail), then step clock backwards and insert an older entry (at head).
	store.Put("newer-at-tail", 1)

	now = base.Add(10 * time.Second)
	store.Put("older-at-head", 2)

	if store.core.ordered {
		t.Fatal("expected non-monotonic clock step to disable tail-only ordering")
	}

	// At t=50s: older-at-head has age 40s (> 30s), newer-at-tail has age -50s (<= 30s).
	if evicted := store.Evict(base.Add(50 * time.Second)); evicted != 1 {
		t.Fatalf("expected 1 evicted via fallback scan, got %d", evicted)
	}
	if _, ok := store.Get("older-at-head"); ok {
		t.Error("expected older-at-head to be evicted despite being at list head")
	}
	if _, ok := store.Get("newer-at-tail"); !ok {
		t.Error("expected newer-at-tail to survive")
	}

	// Verify multi-batch eviction (> defaultEvictBatchSize) in non-monotonic mode.
	now = base
	for i := 0; i < defaultEvictBatchSize*2+50; i++ {
		store.Put(fmt.Sprintf("exp-%d", i), i)
	}
	now = base.Add(100 * time.Second)
	for i := 0; i < 40; i++ {
		store.Put(fmt.Sprintf("keep-%d", i), i)
	}
	wantEvicted := defaultEvictBatchSize*2 + 50
	if evicted := store.Evict(base.Add(50 * time.Second)); evicted != wantEvicted {
		t.Fatalf("expected %d evicted across fallback batches, got %d", wantEvicted, evicted)
	}
	// newer-at-tail + 40 keep-* entries remain.
	if store.Len() != 41 {
		t.Fatalf("expected 41 surviving entries, got %d", store.Len())
	}
	// Calling Evict again when nothing is expired in non-monotonic mode must return 0.
	if evicted := store.Evict(base.Add(50 * time.Second)); evicted != 0 {
		t.Fatalf("expected 0 evicted on second non-monotonic sweep, got %d", evicted)
	}
	if store.Len() != 41 {
		t.Fatalf("expected 41 entries untouched, got %d", store.Len())
	}
}

func TestLRUStore_BackgroundEvictionTicker(t *testing.T) {
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	var unixNano atomic.Int64
	unixNano.Store(base.UnixNano())
	fakeClock := func() time.Time { return time.Unix(0, unixNano.Load()) }

	ttl := time.Second
	store := NewLRUStore[string, int](4, ttl, 5*time.Millisecond, fakeClock)
	t.Cleanup(func() { store.Close() })

	store.Put("k1", 1)
	unixNano.Store(base.Add(ttl + time.Millisecond).UnixNano())

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.Len() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected background ticker to evict k1, len=%d", store.Len())
}

func TestLRUStore_EvictBoundedBatchAndConcurrentRace(t *testing.T) {
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	var unixNano atomic.Int64
	unixNano.Store(base.UnixNano())
	clock := func() time.Time { return time.Unix(0, unixNano.Load()) }

	const capLimit = 2000
	ttl := 10 * time.Second
	store := NewLRUStore[int, int](capLimit, ttl, 0, clock)
	t.Cleanup(func() { store.Close() })

	// Populate > 3 batches of soon-to-expire entries, then advance clock and add fresh entries.
	const expiredCount = defaultEvictBatchSize*3 + 77
	for i := 0; i < expiredCount; i++ {
		store.Put(i, i)
	}
	unixNano.Store(base.Add(ttl + time.Second).UnixNano())
	for i := expiredCount; i < expiredCount+100; i++ {
		store.Put(i, i)
	}

	var wg sync.WaitGroup
	// Concurrent evictors.
	var totalEvicted atomic.Int64
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			totalEvicted.Add(int64(store.Evict(clock())))
		}()
	}
	// Concurrent readers, writers, and Range/Len callers.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				k := expiredCount + (id*200+j)%100
				store.Put(k, j)
				store.Get(k)
				_ = store.Len()
				if j%25 == 0 {
					seen := 0
					store.Range(func(_ int, _ int) bool {
						seen++
						return seen < 10
					})
				}
			}
		}(g)
	}
	wg.Wait()

	if totalEvicted.Load() != int64(expiredCount) {
		t.Fatalf("expected %d total evicted across concurrent callers, got %d", expiredCount, totalEvicted.Load())
	}
	if store.Len() != 100 {
		t.Fatalf("expected 100 fresh entries remaining, got %d", store.Len())
	}
	for i := 0; i < expiredCount; i++ {
		if _, ok := store.Get(i); ok {
			t.Fatalf("expected expired key %d to be removed", i)
		}
	}
}

func TestUnlockedLRU_Direct(t *testing.T) {
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	u := NewUnlockedLRU[int, string](4, 10*time.Second)

	if u.HasExpired(base) {
		t.Fatal("empty cache must not report expired")
	}

	// Fill up to capacity and trigger capacity eviction.
	for i := 0; i < 4; i++ {
		if evicted := u.Put(i, fmt.Sprintf("v%d", i), base.Add(time.Duration(i)*time.Second)); evicted {
			t.Fatalf("unexpected capacity eviction on key %d", i)
		}
	}
	if evicted := u.Put(4, "v4", base.Add(4*time.Second)); !evicted {
		t.Fatal("expected capacity eviction on 5th key")
	}
	if _, ok := u.Get(0); ok {
		t.Fatal("expected oldest key 0 to be capacity-evicted")
	}
	if u.Len() != 4 {
		t.Fatalf("expected len 4, got %d", u.Len())
	}

	// Verify Range early stop.
	visited := 0
	u.Range(func(_ int, _ string) bool {
		visited++
		return visited < 2
	})
	if visited != 2 {
		t.Fatalf("expected Range to stop after 2 items, visited %d", visited)
	}

	// Keys 1, 2, 3, 4 have timestamps +1s, +2s, +3s, +4s.
	// At +12.5s: keys 1 (+1s, age 11.5s) and 2 (+2s, age 10.5s) are expired; 3 and 4 survive.
	checkAt := base.Add(12*time.Second + 500*time.Millisecond)
	if !u.HasExpired(checkAt) {
		t.Fatal("expected HasExpired to be true")
	}
	// Exact batch boundary: maxBatch=2 removes both expired entries and reports more=false.
	n, more := u.EvictBatch(checkAt, 2)
	if n != 2 || more {
		t.Fatalf("expected (2, false), got (%d, %v)", n, more)
	}
	if u.HasExpired(checkAt) {
		t.Fatal("expected no remaining expired entries")
	}

	// maxBatch <= 0 evicts all remaining expired entries in one call.
	n, more = u.EvictBatch(base.Add(30*time.Second), 0)
	if n != 2 || more || u.Len() != 0 {
		t.Fatalf("expected (2, false, len=0), got (%d, %v, len=%d)", n, more, u.Len())
	}
}

func TestLRUStore_ConcurrentEvictions(t *testing.T) {
	const cap = 5
	store := NewLRUStore[string, string](cap, 5*time.Second, 1*time.Second, DefaultGetCurrentTime)
	t.Cleanup(func() { store.Close() })

	const numGoroutines = 10
	const numOperations = 20
	done := make(chan error, numGoroutines)
	defer close(done)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			for j := 0; j < numOperations; j++ {
				key := fmt.Sprintf("goroutine%d_key%d", id, j)
				value := fmt.Sprintf("value%d", j)

				store.Put(key, value)

				if n := store.Len(); n > cap {
					done <- fmt.Errorf("store exceeded capacity: expected at most %d, got %d", cap, n)
					return
				}
			}
			done <- nil
		}(i)
	}

	for i := 0; i < numGoroutines; i++ {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

// appendExpiredAtTail inserts expCount expired entries directly at the tail in O(expCount)
// so each benchmark iteration does not rebuild all 200,000 fresh entries.
func appendExpiredAtTail(s *LRUStore[uint64, struct{}], expCount int, oldTime time.Time) {
	s.Lock()
	defer s.Unlock()
	u := s.core
	for j := 0; j < expCount; j++ {
		k := uint64(10_000_000 + j)
		ent := &entry[uint64, struct{}]{Key: k, lastAccessTime: oldTime}
		ent.next = u.lruList.tail
		ent.prev = u.lruList.tail.prev
		u.lruList.tail.prev.next = ent
		u.lruList.tail.prev = ent
		u.freeTable[k] = ent
	}
}

func BenchmarkLRUStoreEvict(b *testing.B) {
	for _, n := range []int{50000, 100000, 200000} {
		b.Run(fmt.Sprintf("N=%d/expired=0", n), func(b *testing.B) {
			now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
			s := NewLRUStore[uint64, struct{}](n, 20*time.Minute, 0, func() time.Time { return now })
			defer s.Close()
			for i := 0; i < n; i++ {
				s.Put(uint64(i), struct{}{})
			}
			checkAt := now.Add(5 * time.Minute)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.Evict(checkAt)
			}
		})
	}

	const n = 200000
	for _, expCount := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("N=%d/expired=%d", n, expCount), func(b *testing.B) {
			base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
			freshTime := base.Add(25 * time.Minute)
			s := NewLRUStore[uint64, struct{}](n+expCount, 20*time.Minute, 0, func() time.Time { return freshTime })
			defer s.Close()
			for i := 0; i < n-expCount; i++ {
				s.Put(uint64(i), struct{}{})
			}
			evictAt := freshTime.Add(time.Minute)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				appendExpiredAtTail(s, expCount, base)
				b.StartTimer()
				s.Evict(evictAt)
			}
		})
	}
}
