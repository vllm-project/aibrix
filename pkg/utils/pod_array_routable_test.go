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

package utils

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
)

// Plain testing tests (not Ginkgo) so every case runs under -race.

func newReadyPod(name, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
		Status: v1.PodStatus{
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func TestPodArrayRoutableMatchesFilterReadyPod(t *testing.T) {
	now := metav1.Now()
	readyA := newReadyPod("ready-a", "10.0.0.1")
	readyB := newReadyPod("ready-b", "10.0.0.2")
	noIP := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "no-ip"},
		Status:     v1.PodStatus{Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}},
	}
	notReady := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "not-ready"},
		Status:     v1.PodStatus{PodIP: "10.0.0.3", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}}},
	}
	terminating := newReadyPod("terminating", "10.0.0.4")
	terminating.DeletionTimestamp = &now
	draining := newReadyPod("draining", "10.0.0.5")
	draining.Annotations = map[string]string{constants.PodDrainingAnnotationKey: "true"}

	t.Run("nil PodArray", func(t *testing.T) {
		var arr *PodArray
		if got := arr.Routable(); got != nil {
			t.Fatalf("nil.Routable() = %v, want nil", got)
		}
	})

	t.Run("empty PodArray", func(t *testing.T) {
		arr := &PodArray{}
		got := arr.Routable()
		if got != arr {
			t.Fatalf("empty Routable() did not reuse receiver")
		}
		if got.Len() != 0 || got.All() != nil {
			t.Fatalf("empty Routable() = len %d all %v, want 0/nil", got.Len(), got.All())
		}
	})

	t.Run("all ready exact capacity returns self", func(t *testing.T) {
		pods := []*v1.Pod{readyA, readyB}
		arr := &PodArray{Pods: pods}
		got := arr.Routable()
		if got != arr {
			t.Fatalf("all-ready Routable() with cap==len did not return arr itself")
		}
		if !slices.Equal(got.All(), FilterRoutablePods(pods)) {
			t.Fatalf("got %v, want %v", got.All(), FilterRoutablePods(pods))
		}
		if got.Routable() != arr {
			t.Fatalf("Routable().Routable() did not return arr")
		}
	})

	t.Run("mixed readiness states match FilterReadyPod", func(t *testing.T) {
		pods := []*v1.Pod{noIP, readyA, notReady, terminating, draining, readyB}
		published := slices.Clone(pods)
		arr := &PodArray{Pods: pods}

		want := FilterRoutablePods(pods)
		got := arr.Routable()
		if !slices.Equal(got.All(), want) {
			t.Fatalf("Routable().All() = %v, want %v", got.All(), want)
		}
		if got.Len() != CountRoutablePods(pods) {
			t.Fatalf("Routable().Len() = %d, want %d", got.Len(), CountRoutablePods(pods))
		}
		if !slices.Equal(arr.All(), published) {
			t.Fatalf("Routable() mutated arr.Pods")
		}
		// Calling Routable() again returns the exact same cached *PodArray and
		// calling Routable() on the returned snapshot is idempotent.
		if arr.Routable() != got {
			t.Fatalf("second Routable() call returned a different *PodArray")
		}
		if got.Routable() != got {
			t.Fatalf("Routable().Routable() returned a different *PodArray")
		}
	})

	t.Run("all unready", func(t *testing.T) {
		pods := []*v1.Pod{noIP, notReady, terminating, draining}
		arr := &PodArray{Pods: pods}
		got := arr.Routable()
		if got.Len() != 0 {
			t.Fatalf("all-unready Routable().Len() = %d, want 0", got.Len())
		}
		if cap(got.All()) != 0 {
			t.Fatalf("all-unready Routable() has cap %d, want 0", cap(got.All()))
		}
	})

	t.Run("exhaustive 8-pod readiness patterns", func(t *testing.T) {
		const n = 8
		for mask := 0; mask < (1 << n); mask++ {
			pods := make([]*v1.Pod, n)
			for i := 0; i < n; i++ {
				if (mask & (1 << i)) != 0 {
					pods[i] = newReadyPod(fmt.Sprintf("r-%d", i), fmt.Sprintf("10.0.0.%d", i+1))
				} else {
					pods[i] = &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: fmt.Sprintf("u-%d", i)},
					}
				}
			}
			arr := &PodArray{Pods: pods}
			want := FilterRoutablePods(pods)
			got := arr.Routable()
			if !slices.Equal(got.All(), want) {
				t.Fatalf("mask=%08b: got %v, want %v", mask, got.All(), want)
			}
			if got.Len() != CountRoutablePods(pods) {
				t.Fatalf("mask=%08b: Len()=%d, want %d", mask, got.Len(), CountRoutablePods(pods))
			}
			if len(got.All()) != cap(got.All()) {
				t.Fatalf("mask=%08b: got len=%d cap=%d, want clipped", mask, len(got.All()), cap(got.All()))
			}
		}
	})
}

func TestPodArrayRoutableZeroAllocs(t *testing.T) {
	t.Run("all ready exact capacity first and repeat calls", func(t *testing.T) {
		pods := make([]*v1.Pod, 64)
		for i := range pods {
			pods[i] = newReadyPod(fmt.Sprintf("pod-%d", i), fmt.Sprintf("10.0.0.%d", i+1))
		}
		const runs = 50
		arrs := make([]PodArray, runs+1)
		for i := range arrs {
			arrs[i].Pods = pods
		}
		idx := 0
		if allocs := testing.AllocsPerRun(runs, func() {
			arr := &arrs[idx]
			idx++
			if arr.Routable() != arr {
				t.Fatal("expected self")
			}
		}); allocs != 0 {
			t.Fatalf("first Routable() on all-ready snapshot allocated %.1f times, want 0", allocs)
		}

		shared := &PodArray{Pods: pods}
		_ = shared.Routable()
		if allocs := testing.AllocsPerRun(100, func() {
			_ = shared.Routable().All()
		}); allocs != 0 {
			t.Fatalf("steady-state Routable() allocated %.1f times, want 0", allocs)
		}
	})

	t.Run("mixed snapshot steady state", func(t *testing.T) {
		pods := make([]*v1.Pod, 64)
		for i := range pods {
			if i%4 == 0 {
				pods[i] = &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: fmt.Sprintf("u-%d", i)}}
			} else {
				pods[i] = newReadyPod(fmt.Sprintf("r-%d", i), fmt.Sprintf("10.0.0.%d", i+1))
			}
		}
		shared := &PodArray{Pods: pods}
		if got := shared.Routable().Len(); got != 48 {
			t.Fatalf("Len() = %d, want 48", got)
		}
		if allocs := testing.AllocsPerRun(100, func() {
			_ = shared.Routable().All()
		}); allocs != 0 {
			t.Fatalf("steady-state Routable() on mixed snapshot allocated %.1f times, want 0", allocs)
		}
	})
}

func TestPodArrayRoutableIsClipped(t *testing.T) {
	cases := []struct {
		name  string
		build func() []*v1.Pod
	}{
		{
			name: "empty with spare capacity",
			build: func() []*v1.Pod {
				return make([]*v1.Pod, 0, 16)
			},
		},
		{
			name: "all ready with spare capacity",
			build: func() []*v1.Pod {
				pods := make([]*v1.Pod, 0, 16)
				for i := 0; i < 8; i++ {
					pods = append(pods, newReadyPod(fmt.Sprintf("p-%d", i), fmt.Sprintf("10.0.0.%d", i+1)))
				}
				return pods
			},
		},
		{
			name: "mixed with spare capacity",
			build: func() []*v1.Pod {
				pods := make([]*v1.Pod, 0, 16)
				for i := 0; i < 8; i++ {
					if i%2 == 0 {
						pods = append(pods, newReadyPod(fmt.Sprintf("p-%d", i), fmt.Sprintf("10.0.0.%d", i+1)))
					} else {
						pods = append(pods, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: fmt.Sprintf("u-%d", i)}})
					}
				}
				return pods
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pods := tc.build()
			arr := &PodArray{Pods: pods}
			first := arr.Routable().All()
			if len(first) != cap(first) {
				t.Fatalf("Routable().All() returned len %d cap %d, want clipped", len(first), cap(first))
			}
			extra := newReadyPod("extra", "10.0.9.9")
			_ = append(first, extra)
			if again := arr.Routable().All(); slices.Contains(again[:cap(again)], extra) {
				t.Fatalf("appending to Routable().All() polluted the cached slice")
			}
			if slices.Contains(pods[:cap(pods)], extra) {
				t.Fatalf("appending to Routable().All() wrote into spare capacity of Pods")
			}
		})
	}
}

func TestPodArrayRoutableConcurrentRead(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			pods := make([]*v1.Pod, 64)
			for i := range pods {
				if mixed && i%5 == 0 {
					pods[i] = &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: fmt.Sprintf("u-%d", i)}}
				} else {
					pods[i] = newReadyPod(fmt.Sprintf("p-%d", i), fmt.Sprintf("10.0.0.%d", i+1))
				}
			}
			published := slices.Clone(pods)
			want := FilterRoutablePods(pods)
			arr := &PodArray{Pods: pods}

			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					got := arr.Routable()
					if !slices.Equal(got.All(), want) {
						t.Errorf("Routable().All() mismatch")
					}
					if !slices.Equal(arr.All(), published) {
						t.Errorf("arr.All() order changed during concurrent Routable()")
					}
				}()
			}
			close(start)
			wg.Wait()
		})
	}
}

func TestPodArrayRoutablePanicRepeats(t *testing.T) {
	cases := []struct {
		name string
		pods []*v1.Pod
	}{
		{"nil at index 0", []*v1.Pod{nil}},
		{"nil after ready pod", []*v1.Pod{newReadyPod("r-0", "10.0.0.1"), nil}},
		{"nil after unready pod", []*v1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "u-0"}}, nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arr := &PodArray{Pods: tc.pods}
			for i := 0; i < 3; i++ {
				func() {
					defer func() {
						if recover() == nil {
							t.Errorf("call %d did not panic", i)
						}
					}()
					arr.Routable()
				}()
			}
		})
	}
}

// BenchmarkPodArrayRoutableChurn interleaves CustomizedRegistry.Store (which
// invalidates the published *PodArray generation) with simulated requests that
// check routable count and read the routable slice (matching
// validateModelAvailability + selectTargetPod). Comparing cached Routable()
// against uncached CountRoutablePods + FilterRoutablePods makes the break-even
// under pod churn directly observable.
func BenchmarkPodArrayRoutableChurn(b *testing.B) {
	const fleetSize = 2000
	for _, unreadyPct := range []int{0, 5} {
		for _, reqsPerStore := range []int{1, 10, 100, 1000} {
			setup := func() (*CustomizedRegistry[*v1.Pod, *PodArray], []*v1.Pod, []string) {
				reg := NewRegistryWithArrayProvider(func(pods []*v1.Pod) *PodArray {
					return &PodArray{Pods: pods}
				})
				pods := make([]*v1.Pod, fleetSize)
				keys := make([]string, fleetSize)
				for i := 0; i < fleetSize; i++ {
					name := fmt.Sprintf("pod-%04d", i)
					var pod *v1.Pod
					if unreadyPct > 0 && (i*100/fleetSize) < unreadyPct {
						pod = &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}}
					} else {
						pod = newReadyPod(name, fmt.Sprintf("10.0.%d.%d", i/256, i%256))
						pod.Namespace = "ns"
					}
					pods[i] = pod
					keys[i] = GeneratePodKey(pod.Namespace, pod.Name)
					reg.Store(keys[i], pod)
				}
				return reg, pods, keys
			}

			b.Run(fmt.Sprintf("unready=%dpct/reqsPerStore=%d/cached", unreadyPct, reqsPerStore), func(b *testing.B) {
				reg, pods, keys := setup()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if i%reqsPerStore == 0 {
						idx := i % fleetSize
						reg.Store(keys[idx], pods[idx])
					}
					arr := reg.Array()
					if arr.Routable().Len() == 0 {
						b.Fatal("expected routable pods")
					}
					ready := arr.Routable().All()
					if len(ready) == 0 {
						b.Fatal("expected ready slice")
					}
				}
			})

			b.Run(fmt.Sprintf("unready=%dpct/reqsPerStore=%d/uncached", unreadyPct, reqsPerStore), func(b *testing.B) {
				reg, pods, keys := setup()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if i%reqsPerStore == 0 {
						idx := i % fleetSize
						reg.Store(keys[idx], pods[idx])
					}
					arr := reg.Array()
					if CountRoutablePods(arr.All()) == 0 {
						b.Fatal("expected routable pods")
					}
					ready := FilterRoutablePods(arr.All())
					if len(ready) == 0 {
						b.Fatal("expected ready slice")
					}
				}
			})
		}
	}
}
