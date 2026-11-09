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

package utils

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These are plain testing tests rather than Ginkgo specs: utils_test.go, which
// runs the package's Ginkgo suite, is excluded from -race builds.

func snapshotTestPods(n, deployments int) []*v1.Pod {
	pods := make([]*v1.Pod, 0, n)
	for i := 0; i < n; i++ {
		pods = append(pods, &v1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      fmt.Sprintf("pod-%d", i),
			Labels:    map[string]string{DeploymentIdentifier: fmt.Sprintf("deployment-%d", i%deployments)},
		}})
	}
	return pods
}

// The per-model cache hands the same Registry snapshot to every request through
// All(), while the SLO queue indexes it. Indexing must leave that snapshot, and
// the Registry's own copy of it, in the order they were published.
func TestPodArrayIndexingKeepsTheSharedSnapshot(t *testing.T) {
	reg := NewRegistryWithArrayProvider(func(pods []*v1.Pod) *PodArray { return &PodArray{Pods: pods} })
	for _, pod := range snapshotTestPods(8, 2) {
		reg.Store(GeneratePodKey(pod.Namespace, pod.Name), pod)
	}
	arr := reg.Array()
	published := slices.Clone(arr.All())
	registrySnapshot := slices.Clone(reg.Registry.Array())

	deployments := arr.Indexes()

	if !slices.Equal(arr.All(), published) {
		t.Fatalf("indexing reordered the shared pod snapshot")
	}
	if !slices.Equal(reg.Registry.Array(), registrySnapshot) {
		t.Fatalf("indexing reordered the Registry snapshot")
	}
	if got := len(deployments); got != 2 {
		t.Fatalf("Indexes() = %v, want 2 deployments", deployments)
	}
	for _, name := range deployments {
		var want []*v1.Pod
		for _, pod := range published {
			if DeploymentNameFromPod(pod) == name {
				want = append(want, pod)
			}
		}
		if got := arr.ListByIndex(name); !slices.Equal(got, want) {
			t.Fatalf("ListByIndex(%q) = %v, want the deployment's pods in snapshot order %v", name, got, want)
		}
	}
}

// Run with -race: a request goroutine reads the snapshot while another indexes it.
func TestPodArrayConcurrentIndexAndRead(t *testing.T) {
	pods := snapshotTestPods(64, 4)
	published := slices.Clone(pods)
	arr := &PodArray{Pods: pods}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = arr.Indexes()
			_ = arr.ListByIndex("deployment-0")
		}()
		go func() {
			defer wg.Done()
			<-start
			if !slices.Equal(arr.All(), published) {
				t.Error("a reader saw the pod snapshot change while it was indexed")
			}
		}()
	}
	close(start)
	wg.Wait()
}

// Each deployment list is clipped, so a caller appending to one cannot write into
// spare capacity that another caller of ListByIndex sees.
func TestPodArrayListByIndexIsClipped(t *testing.T) {
	arr := &PodArray{Pods: snapshotTestPods(8, 2)}
	first := arr.ListByIndex("deployment-0")
	if len(first) != cap(first) {
		t.Fatalf("ListByIndex returned len %d cap %d, want a clipped slice", len(first), cap(first))
	}
	extra := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "extra"}}
	_ = append(first, extra)
	if again := arr.ListByIndex("deployment-0"); slices.Contains(again[:cap(again)], extra) {
		t.Fatalf("appending to a ListByIndex result changed the index")
	}
}
