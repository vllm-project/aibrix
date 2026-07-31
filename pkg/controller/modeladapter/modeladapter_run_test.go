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

package modeladapter

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// TestModelAdapterReconciler_Run_StopsOnContextCancel verifies that the
// periodical sync loop exits promptly once its context is cancelled. This
// guards against the regression described in vllm-project/aibrix#2067 where
// Run used context.Background() and could leak forever.
func TestModelAdapterReconciler_Run_StopsOnContextCancel(t *testing.T) {
	r := &ModelAdapterReconciler{
		// Large interval so the ticker never fires during the test and Run
		// exits solely via ctx.Done().
		resyncInterval: time.Hour,
		eventCh:        make(chan event.GenericEvent),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Give Run a moment to reach its select loop before cancelling.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Expected: Run returned.
	case <-time.After(time.Second):
		t.Fatal("ModelAdapterReconciler.Run did not exit within 1s after ctx cancel")
	}

	// eventCh must be closed by Run's deferred close(r.eventCh); a second
	// close would panic, and a receive on a closed channel returns zero value
	// immediately.
	if _, ok := <-r.eventCh; ok {
		t.Fatal("expected eventCh to be closed after Run returns")
	}
}

// TestModelAdapterReconciler_EnqueueRespectsCtxCancel verifies that
// enqueueModelAdapters does not block forever when ctx is cancelled while the
// loop is mid-send. Before the fix, the bare `r.eventCh <- e` would block
// indefinitely if the source.Channel consumer had already stopped, causing
// Run to leak and shutdown to hang. The test:
//   - builds a fake client with multiple ModelAdapters,
//   - leaves eventCh unbuffered with no reader,
//   - starts enqueueModelAdapters in a goroutine,
//   - cancels ctx after the first send has started,
//   - asserts the function returns within 1s with a context error.
func TestModelAdapterReconciler_EnqueueRespectsCtxCancel(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := modelv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register scheme: %v", err)
	}

	objs := make([]*modelv1alpha1.ModelAdapter, 0, 5)
	for i := 0; i < 5; i++ {
		objs = append(objs, &modelv1alpha1.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ma-" + string(rune('a'+i)),
				Namespace: "default",
			},
		})
	}

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		builder = builder.WithObjects(o)
	}

	r := &ModelAdapterReconciler{
		Client:  builder.Build(),
		eventCh: make(chan event.GenericEvent), // unbuffered, nobody reading
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.enqueueModelAdapters(ctx)
	}()

	// Allow enqueue to reach its first blocking send before cancelling.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueueModelAdapters did not return within 1s after ctx cancel")
	}
}
