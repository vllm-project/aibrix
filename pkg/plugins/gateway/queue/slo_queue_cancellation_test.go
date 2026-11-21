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

package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestSLOQueueReturnsCanceledHeadBeforeRouting(t *testing.T) {
	const model = "canceled-model"
	st := cache.InitForTest()
	profile := e2eProfile("deployment", 1, 1, 10)
	st.UpdateModelProfile(cache.ModelGPUProfileKey(model, profile.Deployment), profile, true)
	recorder := &routeRecorder{}
	q, err := NewSLOQueue(recordingProvider(recorder, true), model)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := types.NewRoutingContext(requestCtx, "slo", model, "", "canceled-head", "")
	defer ctx.Delete()
	ctx.SetOutputPredictor(&fakeOutputPredictor{reply: 1})
	entry := types.NewQueueEntry(ctx, time.Now())
	defer entry.ReleaseRoutingContext()
	sub := NewSimpleQueue[*types.QueueEntry](4)
	if err := sub.Enqueue(entry, time.Now()); err != nil {
		t.Fatal(err)
	}
	q.subs.Store("head", sub)
	cancel()

	pods := &recordingPodList{deployments: []string{"deployment"}}
	picked, err := q.Peek(time.Now(), pods)
	if err != nil {
		t.Fatal(err)
	}
	if picked != entry {
		t.Fatalf("Peek returned %p, want canceled head %p", picked, entry)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("SLO Peek routed canceled request through %v", recorder.calls)
	}
	if !errors.Is(q.lastCandidateError, context.Canceled) {
		t.Fatalf("candidate error=%v, want cancellation", q.lastCandidateError)
	}
	if _, err := q.Route(ctx, pods); !errors.Is(err, context.Canceled) {
		t.Fatalf("Route error=%v", err)
	}
	removed, err := q.Dequeue(time.Now())
	if err != nil || removed != entry || q.Len() != 0 {
		t.Fatalf("canceled entry not removed: entry=%p err=%v len=%d", removed, err, q.Len())
	}
}
