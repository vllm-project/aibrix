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

package scheduling

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// modelCountCache is a minimal cache.Cache stub shared by the two
// model-count schedulers (binPack and leastAdapters). They only read
// ListModelsByPod, so every other interface method stays nil.
type modelCountCache struct {
	cache.Cache
	models  map[string][]string
	missing map[string]bool // pods that fail ListModelsByPod, e.g. deleted after listing
}

func (c modelCountCache) ListModelsByPod(podName, podNamespace string) ([]string, error) {
	if c.missing[podName] {
		return nil, fmt.Errorf("ListModelsByPod %s/%s: %w", podNamespace, podName, errCacheMiss)
	}
	return c.models[podName], nil
}

func adapters(names ...string) []string {
	return names
}

func readyPod(name string) v1.Pod {
	return v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

var errCacheMiss = errors.New("key does not exist in the cache")

func TestBinPackSchedulerSelectPod(t *testing.T) {
	t.Run("a pod at capacity is passed over", func(t *testing.T) {
		full := make([]string, 10)
		c := modelCountCache{models: map[string][]string{
			"p1": full,
			"p2": adapters("a", "b"),
		}}
		pod, err := NewBinPackScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1"), readyPod("p2")})
		require.NoError(t, err)
		require.Equal(t, "p2", pod.Name)
	})

	// Two eligible pods of different fill pin binPack's documented objective:
	// choose the pod with the least remaining space. With only one eligible pod
	// (as in the case above) an inverted objective would go unnoticed.
	t.Run("prefers the eligible pod with the least remaining capacity", func(t *testing.T) {
		c := modelCountCache{models: map[string][]string{
			"p1": adapters("a", "b", "c", "d", "e", "f", "g", "h"),
			"p2": adapters("a", "b"),
		}}
		pod, err := NewBinPackScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1"), readyPod("p2")})
		require.NoError(t, err)
		require.Equal(t, "p1", pod.Name)
	})

	// A pod deleted between candidate listing and selection must be passed over
	// instead of failing the whole selection, mirroring leastThroughput (#3299)
	// and leastLatency (#3405). The surviving pod has zero models on purpose:
	// dropping the `continue` would let the unscoreable pod win as a phantom
	// zero-model candidate, so this subtest discriminates the skip on its own.
	t.Run("a pod missing from the cache is passed over", func(t *testing.T) {
		c := modelCountCache{
			models:  map[string][]string{"p2": adapters()},
			missing: map[string]bool{"p1": true},
		}
		pod, err := NewBinPackScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1"), readyPod("p2")})
		require.NoError(t, err)
		require.Equal(t, "p2", pod.Name)
	})

	// lastErr must be dropped on success: an error from a skipped pod must not
	// surface when another pod is selected.
	t.Run("an unscoreable pod's error is dropped when another pod is selected", func(t *testing.T) {
		c := modelCountCache{
			models:  map[string][]string{"p2": adapters("a")},
			missing: map[string]bool{"p1": true},
		}
		pod, err := NewBinPackScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1"), readyPod("p2")})
		require.NoError(t, err)
		require.Equal(t, "p2", pod.Name)
	})

	// Every candidate at capacity must produce an error: returning the zero Pod
	// with a nil error used to persist "" as the scheduled pod name.
	t.Run("no pod with capacity is an error, not an empty pod", func(t *testing.T) {
		full := make([]string, 10)
		c := modelCountCache{models: map[string][]string{
			"p1": full,
			"p2": append(append([]string{}, full...), "x"),
		}}
		_, err := NewBinPackScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1"), readyPod("p2")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no ready pod has free adapter capacity")
	})

	t.Run("only unscoreable pods is an error", func(t *testing.T) {
		c := modelCountCache{
			models:  map[string][]string{},
			missing: map[string]bool{"p1": true},
		}
		_, err := NewBinPackScheduler(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no ready pod has free adapter capacity")
		assert.ErrorIs(t, err, errCacheMiss)
	})

	t.Run("no ready pods to schedule on", func(t *testing.T) {
		c := modelCountCache{models: map[string][]string{}}
		_, err := NewBinPackScheduler(c).SelectPod(context.Background(), "m1", nil)
		require.Error(t, err)
		assert.Equal(t, "no ready pods to schedule on", err.Error())
	})
}
