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

package types

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// countingReader answers from live and records the pod names of every call.
type countingReader struct {
	live  map[string]int64
	calls [][]string
	err   error
}

func (c *countingReader) read(pods []*v1.Pod) (map[string]int64, error) {
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	c.calls = append(c.calls, names)
	if c.err != nil {
		return nil, c.err
	}
	out := make(map[string]int64, len(pods))
	for _, pod := range pods {
		if count, ok := c.live[pod.Name]; ok {
			out[pod.Namespace+"/"+pod.Name] = count
		}
	}
	return out, nil
}

func snapshotPods(names ...string) []*v1.Pod {
	pods := make([]*v1.Pod, len(names))
	for i, name := range names {
		pods[i] = &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	}
	return pods
}

func TestRunningRequests_OutsideSnapshotReadsEveryTime(t *testing.T) {
	r := &countingReader{live: map[string]int64{"a": 1, "b": 2}}
	ctx := NewRoutingContext(context.Background(), "least-request", "m", "", "req-live", "")
	defer ctx.Delete()
	pods := snapshotPods("a", "b")

	for i := 0; i < 2; i++ {
		got, err := ctx.RunningRequests(pods, r.read)
		require.NoError(t, err)
		assert.Equal(t, map[string]int64{"ns/a": 1, "ns/b": 2}, got)
	}
	assert.Len(t, r.calls, 2)

	var nilCtx *RoutingContext
	_, err := nilCtx.RunningRequests(pods, r.read)
	require.NoError(t, err)
	assert.Len(t, r.calls, 3, "a nil context reads live")
}

func TestRunningRequests_SnapshotSharesFirstRead(t *testing.T) {
	r := &countingReader{live: map[string]int64{"a": 1, "b": 2, "c": 3}}
	ctx := NewRoutingContext(context.Background(), "least-request", "m", "", "req-snap", "")
	defer ctx.Delete()
	ctx.StartRunningRequestsSnapshot()

	all := snapshotPods("a", "b", "c")
	first, err := ctx.RunningRequests(all, r.read)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"ns/a": 1, "ns/b": 2, "ns/c": 3}, first)
	var got map[string]int64

	// A later change is not seen inside the decision: a subset of the first read, including
	// another object for the same pod (pods are matched by namespace/name, not by pointer),
	// is answered from it.
	r.live["a"] = 9
	for _, pods := range [][]*v1.Pod{{nil, all[0], all[2]}, snapshotPods("a")} {
		got, err = ctx.RunningRequests(pods, r.read)
		require.NoError(t, err)
		assert.Equal(t, int64(1), got["ns/a"])
	}
	assert.Equal(t, [][]string{{"a", "b", "c"}}, r.calls, "a subset of the first read is not read again")

	// A pod outside the first read sends the call to the reader, live, and is not kept.
	got, err = ctx.RunningRequests(snapshotPods("a", "d"), r.read)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"ns/a": 9}, got)
	got, err = ctx.RunningRequests(snapshotPods("b"), r.read)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got["ns/b"])
	assert.Equal(t, [][]string{{"a", "b", "c"}, {"a", "d"}}, r.calls)
	assert.Equal(t, map[string]int64{"ns/a": 1, "ns/b": 2, "ns/c": 3}, first,
		"a result already handed out is never written to")

	// A pod the first read returned no count for is not in the snapshot either, so a call
	// asking for it reads live every time.
	got, err = ctx.RunningRequests(snapshotPods("unknown"), r.read)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Len(t, r.calls, 3)

	ctx.StopRunningRequestsSnapshot()
	got, err = ctx.RunningRequests(snapshotPods("a"), r.read)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"ns/a": 9}, got, "after the decision, reads are live again")
}

func TestRunningRequests_FailedReadIsNotRemembered(t *testing.T) {
	r := &countingReader{live: map[string]int64{"a": 1}, err: errors.New("redis down")}
	ctx := NewRoutingContext(context.Background(), "least-request", "m", "", "req-err", "")
	defer ctx.Delete()
	ctx.StartRunningRequestsSnapshot()

	_, err := ctx.RunningRequests(snapshotPods("a"), r.read)
	require.Error(t, err)

	r.err = nil
	got, err := ctx.RunningRequests(snapshotPods("a"), r.read)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"ns/a": 1}, got)
	assert.Len(t, r.calls, 2, "the read after a failure goes to the reader again")
}

func TestRunningRequests_ResetDropsSnapshot(t *testing.T) {
	r := &countingReader{live: map[string]int64{"a": 1}}
	ctx := NewRoutingContext(context.Background(), "least-request", "m", "", "req-pooled", "")
	ctx.StartRunningRequestsSnapshot()
	_, err := ctx.RunningRequests(snapshotPods("a"), r.read)
	require.NoError(t, err)

	// The pooled context's next request must not answer from this request's counts.
	ctx.reset(context.Background(), "least-request", "m", "", "req-next", "")
	defer ctx.Delete()
	r.live["a"] = 7
	got, err := ctx.RunningRequests(snapshotPods("a"), r.read)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"ns/a": 7}, got)
}

// A routing context can be reached from another goroutine (a queue router's serve loop routes
// a waiting request with it). Reads racing Start and Stop must be safe; run with -race.
func TestRunningRequests_ConcurrentStartStopAndReads(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), "least-request", "m", "", "req-race", "")
	defer ctx.Delete()
	pods := snapshotPods("a", "b")
	read := func(pods []*v1.Pod) (map[string]int64, error) {
		out := make(map[string]int64, len(pods))
		for _, pod := range pods {
			out[pod.Namespace+"/"+pod.Name] = 1
		}
		return out, nil
	}

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				got, err := ctx.RunningRequests(pods, read)
				if assert.NoError(t, err) {
					assert.Equal(t, int64(1), got["ns/a"])
				}
			}
		}()
	}
	for i := 0; i < 500; i++ {
		ctx.StartRunningRequestsSnapshot()
		ctx.StopRunningRequestsSnapshot()
	}
	wg.Wait()
}
