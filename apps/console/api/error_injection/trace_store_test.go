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

package error_injection

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewInMemoryTraceStore tests basic creation of the trace store
func TestNewInMemoryTraceStore(t *testing.T) {
	t.Run("creates store with empty traces", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		assert.NotNil(t, store)
		assert.NotNil(t, store.traces)
		assert.Empty(t, store.traces)
		assert.Equal(t, 0, store.maxTraces)
	})

	t.Run("creates independent stores", func(t *testing.T) {
		store1 := NewInMemoryTraceStore()
		store2 := NewInMemoryTraceStore()
		// Verify they are different instances by modifying one
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}
		err := store1.AppendPoint("job-1", point)
		require.NoError(t, err)

		// store2 should still be empty
		assert.Equal(t, 1, store1.Count())
		assert.Equal(t, 0, store2.Count())
	})
}

// TestAppendPoint tests the AppendPoint method
func TestAppendPoint(t *testing.T) {
	t.Run("append to new job creates trace", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		now := time.Now().UTC()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: now,
			Triggered: false,
		}

		err := store.AppendPoint("job-1", point)
		require.NoError(t, err)

		trace, err := store.Get("job-1")
		require.NoError(t, err)
		assert.Equal(t, "job-1", trace.JobID)
		assert.Equal(t, now, trace.StartTime)
		assert.Equal(t, now, trace.EndTime)
		assert.Len(t, trace.Points, 1)
		assert.Equal(t, point, trace.Points[0])
	})

	t.Run("append to existing job", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		now := time.Now().UTC()
		point1 := PointRecord{
			PointID:   "test.point1",
			Timestamp: now,
			Triggered: false,
		}
		point2 := PointRecord{
			PointID:   "test.point2",
			Timestamp: now.Add(1 * time.Second),
			Triggered: true,
		}

		// Append first point
		err := store.AppendPoint("job-1", point1)
		require.NoError(t, err)

		// Append second point
		err = store.AppendPoint("job-1", point2)
		require.NoError(t, err)

		trace, err := store.Get("job-1")
		require.NoError(t, err)
		assert.Len(t, trace.Points, 2)
		assert.Equal(t, point1, trace.Points[0])
		assert.Equal(t, point2, trace.Points[1])
	})

	t.Run("StartTime/EndTime tracking", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		baseTime := time.Now().UTC()

		// Points added in non-chronological order
		point1 := PointRecord{
			PointID:   "test.point1",
			Timestamp: baseTime.Add(2 * time.Second),
		}
		point2 := PointRecord{
			PointID:   "test.point2",
			Timestamp: baseTime,
		}
		point3 := PointRecord{
			PointID:   "test.point3",
			Timestamp: baseTime.Add(5 * time.Second),
		}

		err := store.AppendPoint("job-1", point1)
		require.NoError(t, err)
		err = store.AppendPoint("job-1", point2)
		require.NoError(t, err)
		err = store.AppendPoint("job-1", point3)
		require.NoError(t, err)

		trace, err := store.Get("job-1")
		require.NoError(t, err)

		// StartTime should be the earliest
		assert.Equal(t, baseTime, trace.StartTime)
		// EndTime should be the latest
		assert.Equal(t, baseTime.Add(5*time.Second), trace.EndTime)
	})

	t.Run("thread safety with concurrent appends", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		numGoroutines := 100
		numAppendsPerGoroutine := 10

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(jobNum int) {
				defer wg.Done()
				for j := 0; j < numAppendsPerGoroutine; j++ {
					point := PointRecord{
						PointID:   "test.point",
						Timestamp: time.Now().UTC(),
						Triggered: false,
					}
					err := store.AppendPoint("job-concurrent", point)
					assert.NoError(t, err)
				}
			}(i)
		}

		wg.Wait()

		trace, err := store.Get("job-concurrent")
		require.NoError(t, err)
		// All points should be appended
		assert.Len(t, trace.Points, numGoroutines*numAppendsPerGoroutine)
	})

	t.Run("concurrent appends to different jobs", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		numJobs := 50

		var wg sync.WaitGroup
		wg.Add(numJobs)

		for i := 0; i < numJobs; i++ {
			go func(jobNum int) {
				defer wg.Done()
				jobID := string(rune('a'+jobNum%26)) + string(rune('a'+jobNum/26))
				point := PointRecord{
					PointID:   "test.point",
					Timestamp: time.Now().UTC(),
				}
				err := store.AppendPoint(jobID, point)
				assert.NoError(t, err)
			}(i)
		}

		wg.Wait()

		// All jobs should be created
		assert.Equal(t, numJobs, store.Count())
	})
}

// TestGet tests the Get method
func TestGet(t *testing.T) {
	t.Run("get existing trace", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}
		err := store.AppendPoint("job-1", point)
		require.NoError(t, err)

		trace, err := store.Get("job-1")
		require.NoError(t, err)
		assert.NotNil(t, trace)
		assert.Equal(t, "job-1", trace.JobID)
	})

	t.Run("get non-existent trace returns error", func(t *testing.T) {
		store := NewInMemoryTraceStore()

		trace, err := store.Get("non-existent")
		assert.Error(t, err)
		assert.Equal(t, ErrTraceNotFound, err)
		assert.Nil(t, trace)
	})

	t.Run("get after delete returns error", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}
		err := store.AppendPoint("job-1", point)
		require.NoError(t, err)

		err = store.Delete("job-1")
		require.NoError(t, err)

		trace, err := store.Get("job-1")
		assert.Error(t, err)
		assert.Equal(t, ErrTraceNotFound, err)
		assert.Nil(t, trace)
	})
}

// TestDelete tests the Delete method
func TestDelete(t *testing.T) {
	t.Run("delete existing trace", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}
		err := store.AppendPoint("job-1", point)
		require.NoError(t, err)
		assert.Equal(t, 1, store.Count())

		err = store.Delete("job-1")
		require.NoError(t, err)
		assert.Equal(t, 0, store.Count())

		// Verify it's actually deleted
		_, err = store.Get("job-1")
		assert.Equal(t, ErrTraceNotFound, err)
	})

	t.Run("delete non-existent trace returns no error", func(t *testing.T) {
		store := NewInMemoryTraceStore()

		err := store.Delete("non-existent")
		assert.NoError(t, err)
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}
		err := store.AppendPoint("job-1", point)
		require.NoError(t, err)

		// Delete twice
		err = store.Delete("job-1")
		require.NoError(t, err)
		err = store.Delete("job-1")
		require.NoError(t, err)
	})
}

// TestList tests the List method
func TestList(t *testing.T) {
	t.Run("list with limit", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		baseTime := time.Now().UTC()

		// Add 5 traces
		for i := 0; i < 5; i++ {
			point := PointRecord{
				PointID:   "test.point",
				Timestamp: baseTime.Add(time.Duration(i) * time.Second),
			}
			err := store.AppendPoint(string(rune('a'+i)), point)
			require.NoError(t, err)
		}

		// List with limit 3
		traces, err := store.List(3)
		require.NoError(t, err)
		assert.Len(t, traces, 3)
	})

	t.Run("list returns traces sorted by StartTime newest first", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		baseTime := time.Now().UTC()

		// Add traces with different start times
		jobIDs := []string{"job-old", "job-middle", "job-new"}
		times := []time.Duration{0, 2 * time.Second, 5 * time.Second}

		for i, jobID := range jobIDs {
			point := PointRecord{
				PointID:   "test.point",
				Timestamp: baseTime.Add(times[i]),
			}
			err := store.AppendPoint(jobID, point)
			require.NoError(t, err)
		}

		traces, err := store.List(10)
		require.NoError(t, err)
		assert.Len(t, traces, 3)

		// Should be sorted newest first
		assert.Equal(t, "job-new", traces[0].JobID)
		assert.Equal(t, "job-middle", traces[1].JobID)
		assert.Equal(t, "job-old", traces[2].JobID)

		// Verify times are in descending order
		for i := 1; i < len(traces); i++ {
			assert.True(t, traces[i-1].StartTime.After(traces[i].StartTime) ||
				traces[i-1].StartTime.Equal(traces[i].StartTime))
		}
	})

	t.Run("list empty store", func(t *testing.T) {
		store := NewInMemoryTraceStore()

		traces, err := store.List(10)
		require.NoError(t, err)
		assert.Empty(t, traces)
	})

	t.Run("list with zero or negative limit returns all", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}

		for i := 0; i < 3; i++ {
			err := store.AppendPoint(string(rune('a'+i)), point)
			require.NoError(t, err)
		}

		// Limit 0 should return all
		traces, err := store.List(0)
		require.NoError(t, err)
		assert.Len(t, traces, 3)

		// Negative limit should return all
		traces, err = store.List(-1)
		require.NoError(t, err)
		assert.Len(t, traces, 3)
	})

	t.Run("list limit larger than count returns all", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}

		for i := 0; i < 3; i++ {
			err := store.AppendPoint(string(rune('a'+i)), point)
			require.NoError(t, err)
		}

		traces, err := store.List(100)
		require.NoError(t, err)
		assert.Len(t, traces, 3)
	})
}

// TestTraceStoreWithLimit tests LRU eviction
func TestTraceStoreWithLimit(t *testing.T) {
	t.Run("create store with limit", func(t *testing.T) {
		store := NewInMemoryTraceStoreWithLimit(3)
		assert.NotNil(t, store)
		assert.Equal(t, 3, store.maxTraces)
	})

	t.Run("oldest trace is evicted when limit exceeded", func(t *testing.T) {
		store := NewInMemoryTraceStoreWithLimit(3)
		baseTime := time.Now().UTC()

		// Add 4 traces with increasing start times
		for i := 0; i < 4; i++ {
			point := PointRecord{
				PointID:   "test.point",
				Timestamp: baseTime.Add(time.Duration(i) * time.Second),
			}
			jobID := string(rune('a' + i))
			err := store.AppendPoint(jobID, point)
			require.NoError(t, err)
		}

		// Should have exactly 3 traces (limit)
		assert.Equal(t, 3, store.Count())

		// Oldest trace (a) should have been evicted
		_, err := store.Get("a")
		assert.Equal(t, ErrTraceNotFound, err)

		// Newer traces should still exist
		for _, jobID := range []string{"b", "c", "d"} {
			_, err := store.Get(jobID)
			require.NoError(t, err, "trace %s should exist", jobID)
		}
	})

	t.Run("eviction maintains limit with many additions", func(t *testing.T) {
		store := NewInMemoryTraceStoreWithLimit(5)
		baseTime := time.Now().UTC()

		// Add 10 traces
		for i := 0; i < 10; i++ {
			point := PointRecord{
				PointID:   "test.point",
				Timestamp: baseTime.Add(time.Duration(i) * time.Second),
			}
			jobID := string(rune('0' + i))
			err := store.AppendPoint(jobID, point)
			require.NoError(t, err)
		}

		// Should have exactly 5 traces (limit)
		assert.Equal(t, 5, store.Count())

		// Only the newest 5 should exist (5-9)
		for i := 0; i < 5; i++ {
			jobID := string(rune('0' + i))
			_, err := store.Get(jobID)
			assert.Equal(t, ErrTraceNotFound, err, "trace %s should have been evicted", jobID)
		}
		for i := 5; i < 10; i++ {
			jobID := string(rune('0' + i))
			_, err := store.Get(jobID)
			require.NoError(t, err, "trace %s should exist", jobID)
		}
	})

	t.Run("no eviction when limit is zero", func(t *testing.T) {
		store := NewInMemoryTraceStoreWithLimit(0)
		baseTime := time.Now().UTC()

		// Add many traces
		for i := 0; i < 100; i++ {
			point := PointRecord{
				PointID:   "test.point",
				Timestamp: baseTime.Add(time.Duration(i) * time.Second),
			}
			jobID := string(rune('a' + (i % 26)))
			if i >= 26 {
				jobID = string(rune('a'+(i/26)%26)) + string(rune('a'+i%26))
			}
			err := store.AppendPoint(jobID, point)
			require.NoError(t, err)
		}

		// All traces should still exist (no limit)
		assert.Equal(t, 100, store.Count())
	})
}

// TestCount tests the Count method
func TestCount(t *testing.T) {
	t.Run("count empty store", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		assert.Equal(t, 0, store.Count())
	})

	t.Run("count after additions", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}

		for i := 0; i < 5; i++ {
			err := store.AppendPoint(string(rune('a'+i)), point)
			require.NoError(t, err)
		}

		assert.Equal(t, 5, store.Count())
	})

	t.Run("count after deletions", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}

		// Add 3 traces
		for i := 0; i < 3; i++ {
			err := store.AppendPoint(string(rune('a'+i)), point)
			require.NoError(t, err)
		}
		assert.Equal(t, 3, store.Count())

		// Delete 2 traces
		err := store.Delete("a")
		require.NoError(t, err)
		err = store.Delete("b")
		require.NoError(t, err)

		assert.Equal(t, 1, store.Count())
	})

	t.Run("count is thread-safe", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		numGoroutines := 50

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(jobNum int) {
				defer wg.Done()
				jobID := string(rune('a'+jobNum%26)) + string(rune('a'+jobNum/26))
				point := PointRecord{
					PointID:   "test.point",
					Timestamp: time.Now().UTC(),
				}
				err := store.AppendPoint(jobID, point)
				assert.NoError(t, err)
			}(i)
		}

		wg.Wait()
		assert.Equal(t, numGoroutines, store.Count())
	})
}

// TestClear tests the Clear method
func TestClear(t *testing.T) {
	t.Run("clear empty store", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		store.Clear()
		assert.Equal(t, 0, store.Count())
	})

	t.Run("clear removes all traces", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}

		// Add traces
		for i := 0; i < 5; i++ {
			err := store.AppendPoint(string(rune('a'+i)), point)
			require.NoError(t, err)
		}
		assert.Equal(t, 5, store.Count())

		// Clear
		store.Clear()
		assert.Equal(t, 0, store.Count())

		// Verify all traces are gone
		for i := 0; i < 5; i++ {
			_, err := store.Get(string(rune('a' + i)))
			assert.Equal(t, ErrTraceNotFound, err)
		}
	})

	t.Run("clear allows new additions", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}

		// Add and clear
		err := store.AppendPoint("job-1", point)
		require.NoError(t, err)
		store.Clear()

		// Add new trace
		err = store.AppendPoint("job-2", point)
		require.NoError(t, err)

		// Verify new trace exists
		trace, err := store.Get("job-2")
		require.NoError(t, err)
		assert.Equal(t, "job-2", trace.JobID)

		// Verify old trace doesn't exist
		_, err = store.Get("job-1")
		assert.Equal(t, ErrTraceNotFound, err)
	})
}

// TestThreadSafety tests concurrent operations across different methods
func TestThreadSafety(t *testing.T) {
	t.Run("concurrent append, get, delete, list", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		numOps := 100
		baseTime := time.Now().UTC()

		var wg sync.WaitGroup

		// Concurrent appends
		wg.Add(numOps)
		for i := 0; i < numOps; i++ {
			go func(i int) {
				defer wg.Done()
				point := PointRecord{
					PointID:   "test.point",
					Timestamp: baseTime.Add(time.Duration(i) * time.Millisecond),
				}
				err := store.AppendPoint(string(rune('a'+i%26)), point)
				assert.NoError(t, err)
			}(i)
		}

		// Concurrent gets
		wg.Add(numOps)
		for i := 0; i < numOps; i++ {
			go func(i int) {
				defer wg.Done()
				jobID := string(rune('a' + i%26))
				_, _ = store.Get(jobID) // May or may not exist, that's fine
			}(i)
		}

		// Concurrent deletes (on different jobs than appends)
		wg.Add(numOps / 2)
		for i := 0; i < numOps/2; i++ {
			go func(i int) {
				defer wg.Done()
				jobID := string(rune('A' + i%26))
				err := store.Delete(jobID)
				assert.NoError(t, err)
			}(i)
		}

		// Concurrent lists
		wg.Add(numOps / 2)
		for i := 0; i < numOps/2; i++ {
			go func() {
				defer wg.Done()
				_, err := store.List(10)
				assert.NoError(t, err)
			}()
		}

		wg.Wait()

		// Final count should be consistent
		count := store.Count()
		assert.GreaterOrEqual(t, count, 0)
		assert.LessOrEqual(t, count, 26) // Max 26 jobs (a-z)
	})

	t.Run("concurrent count and clear", func(t *testing.T) {
		store := NewInMemoryTraceStore()
		point := PointRecord{
			PointID:   "test.point",
			Timestamp: time.Now().UTC(),
		}

		// Add initial traces
		for i := 0; i < 10; i++ {
			err := store.AppendPoint(string(rune('a'+i)), point)
			require.NoError(t, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		// Concurrent count
		go func() {
			defer wg.Done()
			_ = store.Count()
		}()

		// Concurrent clear
		go func() {
			defer wg.Done()
			store.Clear()
		}()

		wg.Wait()

		// After clear, count should be 0
		assert.Equal(t, 0, store.Count())
	})
}
