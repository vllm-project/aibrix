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
)

// Plain testing tests (not Ginkgo) so every case runs under -race.

func TestPodArrayKeySetAndByKey(t *testing.T) {
	t.Run("nil and empty PodArray", func(t *testing.T) {
		var nilArr *PodArray
		if nilArr.KeySet() != nil || nilArr.ByKey() != nil {
			t.Fatalf("nil PodArray must return nil maps")
		}
		empty := &PodArray{}
		if empty.KeySet() != nil || empty.ByKey() != nil {
			t.Fatalf("empty PodArray must return nil maps")
		}
	})

	t.Run("independent lazy caches and exact mapping", func(t *testing.T) {
		pods := make([]*v1.Pod, 16)
		for i := range pods {
			pods[i] = newReadyPod(fmt.Sprintf("pod-%02d", i), fmt.Sprintf("10.0.0.%d", i+1))
			pods[i].Namespace = fmt.Sprintf("ns-%d", i%3)
		}
		published := slices.Clone(pods)
		arr := &PodArray{Pods: pods}

		// Calling Routable() alone builds neither map (non-prefix-cache models pay zero map builds).
		_ = arr.Routable()
		if arr.keySet != nil || arr.byKey != nil {
			t.Fatalf("Routable() must not eagerly build KeySet or ByKey")
		}

		// Calling KeySet() builds only keySet, not byKey.
		keys := arr.KeySet()
		if arr.byKey != nil {
			t.Fatalf("KeySet() must not eagerly build ByKey")
		}
		if len(keys) != len(pods) {
			t.Fatalf("len(KeySet) = %d, want %d", len(keys), len(pods))
		}
		for _, pod := range pods {
			k := GeneratePodKey(pod.Namespace, pod.Name)
			if _, ok := keys[k]; !ok {
				t.Fatalf("KeySet missing key %q", k)
			}
		}

		// Calling ByKey() on a fresh PodArray builds only byKey, not keySet.
		arr2 := &PodArray{Pods: pods}
		byKey2 := arr2.ByKey()
		if arr2.keySet != nil {
			t.Fatalf("ByKey() must not eagerly build KeySet")
		}
		if len(byKey2) != len(pods) {
			t.Fatalf("len(ByKey) = %d, want %d", len(byKey2), len(pods))
		}
		for _, pod := range pods {
			k := GeneratePodKey(pod.Namespace, pod.Name)
			if got := byKey2[k]; got != pod {
				t.Fatalf("ByKey[%q] = %v, want %v", k, got, pod)
			}
		}

		// Steady-state calls allocate nothing.
		_ = arr.ByKey()
		if allocs := testing.AllocsPerRun(100, func() {
			if len(arr.KeySet()) != len(pods) || len(arr.ByKey()) != len(pods) {
				t.Fatal("unexpected map length")
			}
		}); allocs != 0 {
			t.Fatalf("steady-state KeySet/ByKey allocated %.1f times, want 0", allocs)
		}

		if !slices.Equal(arr.All(), published) {
			t.Fatalf("KeySet/ByKey mutated arr.Pods")
		}
	})
}

func TestPodArrayKeyIndexConcurrentRead(t *testing.T) {
	pods := make([]*v1.Pod, 64)
	for i := range pods {
		pods[i] = newReadyPod(fmt.Sprintf("pod-%03d", i), fmt.Sprintf("10.0.0.%d", i+1))
	}
	published := slices.Clone(pods)
	arr := &PodArray{Pods: pods}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys := arr.KeySet()
			byKey := arr.ByKey()
			if len(keys) != len(pods) || len(byKey) != len(pods) {
				t.Errorf("unexpected map sizes: keys=%d byKey=%d", len(keys), len(byKey))
				return
			}
			for _, pod := range pods {
				k := GeneratePodKey(pod.Namespace, pod.Name)
				if _, ok := keys[k]; !ok {
					t.Errorf("KeySet missing %q", k)
					return
				}
				if byKey[k] != pod {
					t.Errorf("ByKey[%q] mismatch", k)
					return
				}
			}
			if !slices.Equal(arr.All(), published) {
				t.Errorf("arr.All() mutated during concurrent KeySet/ByKey")
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestPodArrayKeyIndexPanicRepeats(t *testing.T) {
	cases := []struct {
		name string
		pods []*v1.Pod
	}{
		{"nil at index 0", []*v1.Pod{nil}},
		{"nil after valid pod", []*v1.Pod{newReadyPod("pod-0", "10.0.0.1"), nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arr := &PodArray{Pods: tc.pods}
			for i := 0; i < 3; i++ {
				func() {
					defer func() {
						if recover() == nil {
							t.Errorf("KeySet call %d did not panic", i)
						}
					}()
					_ = arr.KeySet()
				}()
				func() {
					defer func() {
						if recover() == nil {
							t.Errorf("ByKey call %d did not panic", i)
						}
					}()
					_ = arr.ByKey()
				}()
			}
		})
	}
}

// TestCustomizedRegistryInvalidatesKeyIndexOnStoreAndDelete pins the cache
// boundary invariant: Store and Delete publish a fresh *PodArray so Routable,
// KeySet, and ByKey never serve stale pods across informer updates while older
// snapshots remain immutable.
func TestCustomizedRegistryInvalidatesKeyIndexOnStoreAndDelete(t *testing.T) {
	reg := NewRegistryWithArrayProvider(func(pods []*v1.Pod) *PodArray {
		return &PodArray{Pods: pods}
	})
	podA := newReadyPod("pod-a", "10.0.0.1")
	podB := newReadyPod("pod-b", "10.0.0.2")
	keyA := GeneratePodKey(podA.Namespace, podA.Name)
	keyB := GeneratePodKey(podB.Namespace, podB.Name)
	reg.Store(keyA, podA)
	reg.Store(keyB, podB)

	gen1 := reg.Array()
	if gen1.Routable().Len() != 2 || len(gen1.KeySet()) != 2 || gen1.ByKey()[keyA] != podA {
		t.Fatalf("unexpected gen1 caches: routable=%d keys=%d", gen1.Routable().Len(), len(gen1.KeySet()))
	}

	// Replace podA with an unready pod object (same key): new snapshot must reflect it.
	podAUnready := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: podA.Namespace, Name: podA.Name}}
	reg.Store(keyA, podAUnready)
	gen2 := reg.Array()
	if gen2 == gen1 {
		t.Fatalf("Store did not publish a new *PodArray")
	}
	if gen2.Routable().Len() != 1 || gen2.ByKey()[keyA] != podAUnready {
		t.Fatalf("gen2 did not reflect updated podA: routable=%d byKey=%v", gen2.Routable().Len(), gen2.ByKey()[keyA])
	}
	if _, ok := gen2.Routable().KeySet()[keyA]; ok {
		t.Fatalf("gen2.Routable().KeySet() must not contain unready podA")
	}
	// Earlier snapshot stays untouched for in-flight requests.
	if gen1.Routable().Len() != 2 || gen1.ByKey()[keyA] != podA {
		t.Fatalf("gen1 was mutated by later Store")
	}

	// Delete podB: new snapshot drops keyB cleanly.
	reg.Delete(keyB)
	gen3 := reg.Array()
	if gen3.Routable().Len() != 0 || len(gen3.KeySet()) != 1 {
		t.Fatalf("gen3 after Delete: routable=%d keys=%d, want 0/1", gen3.Routable().Len(), len(gen3.KeySet()))
	}
	if _, ok := gen3.KeySet()[keyB]; ok {
		t.Fatalf("gen3.KeySet() still contains deleted keyB")
	}
	if gen3.ByKey()[keyB] != nil {
		t.Fatalf("gen3.ByKey() still contains deleted keyB")
	}
}

// BenchmarkPodArrayKeyIndexChurn interleaves CustomizedRegistry.Store (which
// invalidates the published *PodArray generation) with simulated prefix-cache
// FindCandidates requests at N=2000. Comparing cached (arr.KeySet + arr.ByKey)
// against uncached (per-request ready map + O(N) pod key scan, as before #3545c)
// shows the break-even under pod churn, while the non_prefix arm confirms
// non-prefix-cache models pay zero map builds per generation.
func BenchmarkPodArrayKeyIndexChurn(b *testing.B) {
	const fleetSize = 2000
	setup := func() (*CustomizedRegistry[*v1.Pod, *PodArray], []*v1.Pod, []string) {
		reg := NewRegistryWithArrayProvider(func(pods []*v1.Pod) *PodArray {
			return &PodArray{Pods: pods}
		})
		pods := make([]*v1.Pod, fleetSize)
		keys := make([]string, fleetSize)
		for i := 0; i < fleetSize; i++ {
			pod := newReadyPod(fmt.Sprintf("pod-%04d", i), fmt.Sprintf("10.0.%d.%d", i/256, i%256))
			pod.Namespace = "ns"
			pods[i] = pod
			keys[i] = GeneratePodKey(pod.Namespace, pod.Name)
			reg.Store(keys[i], pod)
		}
		return reg, pods, keys
	}

	for _, reqsPerStore := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("reqsPerStore=%d/cached_keyset_and_bykey", reqsPerStore), func(b *testing.B) {
			reg, pods, keys := setup()
			targetKey := keys[0]
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%reqsPerStore == 0 {
					idx := i % fleetSize
					reg.Store(keys[idx], pods[idx])
				}
				arr := reg.Array().Routable()
				ks := arr.KeySet()
				if _, ok := ks[targetKey]; !ok {
					b.Fatal("missing key")
				}
				if arr.ByKey()[targetKey] == nil {
					b.Fatal("missing pod")
				}
			}
		})

		b.Run(fmt.Sprintf("reqsPerStore=%d/cached_keyset_only", reqsPerStore), func(b *testing.B) {
			reg, pods, keys := setup()
			targetKey := keys[0]
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%reqsPerStore == 0 {
					idx := i % fleetSize
					reg.Store(keys[idx], pods[idx])
				}
				arr := reg.Array().Routable()
				if _, ok := arr.KeySet()[targetKey]; !ok {
					b.Fatal("missing key")
				}
			}
		})

		b.Run(fmt.Sprintf("reqsPerStore=%d/uncached_per_request", reqsPerStore), func(b *testing.B) {
			reg, pods, keys := setup()
			targetKey := keys[0]
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%reqsPerStore == 0 {
					idx := i % fleetSize
					reg.Store(keys[idx], pods[idx])
				}
				arr := reg.Array().Routable()
				all := arr.All()
				ready := make(map[string]struct{}, len(all))
				for _, pod := range all {
					ready[GeneratePodKey(pod.Namespace, pod.Name)] = struct{}{}
				}
				if _, ok := ready[targetKey]; !ok {
					b.Fatal("missing key")
				}
				// Mirrors scoreAll's O(N) scores/scored allocations and key scan.
				scores := make([]float64, len(all))
				scored := make([]bool, len(all))
				for j, pod := range all {
					if GeneratePodKey(pod.Namespace, pod.Name) == targetKey {
						scores[j] = 100
					}
					scored[j] = true
				}
				if len(scores) == 0 || !scored[0] {
					b.Fatal("unexpected empty scores")
				}
			}
		})

		b.Run(fmt.Sprintf("reqsPerStore=%d/non_prefix_routable_only", reqsPerStore), func(b *testing.B) {
			reg, pods, keys := setup()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%reqsPerStore == 0 {
					idx := i % fleetSize
					reg.Store(keys[idx], pods[idx])
				}
				arr := reg.Array().Routable()
				if arr.Len() == 0 {
					b.Fatal("empty")
				}
			}
		})
	}
}
