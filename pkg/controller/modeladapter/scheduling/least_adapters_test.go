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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
)

func TestLeastAdaptersSelectPod(t *testing.T) {
	t.Run("picks the pod with the fewest adapters", func(t *testing.T) {
		c := modelCountCache{models: map[string][]string{
			"p1": adapters("a", "b", "c"),
			"p2": adapters("a"),
		}}
		pod, err := NewLeastAdapters(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1"), readyPod("p2")})
		require.NoError(t, err)
		require.Equal(t, "p2", pod.Name)
	})

	// A pod deleted between candidate listing and selection must be passed over,
	// mirroring leastThroughput (#3299) and leastLatency (#3405). The surviving
	// pod has zero models, so removing the `continue` would let the unscoreable
	// pod win as a phantom zero-model candidate.
	t.Run("a pod missing from the cache is passed over", func(t *testing.T) {
		c := modelCountCache{
			models:  map[string][]string{"p2": adapters()},
			missing: map[string]bool{"p1": true},
		}
		pod, err := NewLeastAdapters(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1"), readyPod("p2")})
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
		pod, err := NewLeastAdapters(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1"), readyPod("p2")})
		require.NoError(t, err)
		require.Equal(t, "p2", pod.Name)
	})

	// The twin of binPack's "only unscoreable pods is an error": the
	// `no ready pod has adapter info: %w` branch must be covered, and the
	// ErrorIs assertion pins that the cache error stays wrapped with %w
	// (a bare %v would silently lose errors.Is capability).
	t.Run("every pod unscoreable is an error", func(t *testing.T) {
		c := modelCountCache{
			models:  map[string][]string{},
			missing: map[string]bool{"p1": true, "p2": true},
		}
		_, err := NewLeastAdapters(c).SelectPod(context.Background(), "m1", []v1.Pod{readyPod("p1"), readyPod("p2")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no ready pod has adapter info")
		assert.ErrorIs(t, err, errCacheMiss)
	})

	t.Run("no ready pods to schedule on", func(t *testing.T) {
		c := modelCountCache{models: map[string][]string{}}
		_, err := NewLeastAdapters(c).SelectPod(context.Background(), "m1", nil)
		require.Error(t, err)
		assert.Equal(t, "no ready pods to schedule on", err.Error())
	})
}
