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
	"slices"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestPodArrayIndexPreservesPublishedSnapshot(t *testing.T) {
	for round := 0; round < 16; round++ {
		pods := []*v1.Pod{getPodWithDeployment("a"), getPodWithDeployment("b"), getPodWithDeployment("a")}
		original := slices.Clone(pods)
		arr := &PodArray{Pods: pods}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for reader := 0; reader < 4; reader++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < 32; i++ {
					if !slices.Equal(arr.All(), original) {
						t.Error("indexing changed the published pod snapshot")
						return
					}
					if !slices.Equal(arr.Indexes(), []string{"a", "b"}) {
						t.Error("incorrect deployment index")
						return
					}
					if !slices.Equal(arr.ListByIndex("a"), []*v1.Pod{original[0], original[2]}) {
						t.Error("incorrect deployment members")
						return
					}
				}
			}()
		}
		close(start)
		wg.Wait()
	}
}
