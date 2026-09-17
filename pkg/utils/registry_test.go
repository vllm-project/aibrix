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
	"sync"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	testKeys = []string{"key1", "key2", "key3"}
)

func testPod(name string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

var _ = Describe("Registry", func() {
	Context("Registry", func() {
		var registry *Registry[string]

		BeforeEach(func() {
			registry = NewRegistry[string]()
		})

		It("should nil registry status accessible", func() {
			var registry *Registry[string]
			Expect(registry.Len()).To(Equal(0))
			Expect(registry.Array()).To(BeEmpty())
		})

		It("verify empty registry status", func() {
			Expect(registry.Len()).To(Equal(0))
			Expect(registry.Array()).To(BeEmpty())

			Expect(func() { registry.Delete("testItem") }).NotTo(Panic())
		})

		It("should reflect registry updates after added new element", func() {
			// Add an item to the registry
			item := testKeys[0]
			registry.Store(item, item)

			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array()).To(ContainElement(item))

			// Remove the item from the registry
			item2 := testKeys[1]
			registry.Store(item2, item2)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(2))
			Expect(registry.Array()).To(ContainElement(item))
			Expect(registry.Array()).To(ContainElement(item2))
		})

		It("should reflect registry updates after element changes", func() {
			// Add an item to the registry
			item := testKeys[0]
			registry.Store(item, item)

			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(len(registry.Array())).To(Equal(1))
			Expect(registry.Array()).To(ContainElement(item))

			// Remove the item from the registry
			item2 := testKeys[1]
			registry.Store(item, item2)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(len(registry.Array())).To(Equal(1))
			Expect(registry.Array()).To(ContainElement(item2))
		})

		It("should reflect registry updates after removal element", func() {
			// Add an item to the registry
			item := testKeys[0]
			registry.Store(item, item)
			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(len(registry.Array())).To(Equal(1))
			Expect(registry.Array()).To(ContainElement(item))

			// Remove the item from the registry
			registry.Delete(item)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(0))
			Expect(len(registry.Array())).To(Equal(0))
			Expect(registry.Array()).NotTo(ContainElement(item))

			// Check 0 length leads same result as nil
			item2 := testKeys[1]
			registry.Store(item, item)
			registry.Store(item2, item2)
			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(2))
			Expect(len(registry.Array())).To(Equal(2))
			Expect(registry.Array()).To(ContainElement(item))
			Expect(registry.Array()).To(ContainElement(item2))
		})

		It("should return the cached array on a subsequent updateArrayLocked call", func() {
			// Add an item to the registry
			registry.Store(testKeys[0], testKeys[0])
			registry.values.Store(nil)

			arr, reconstructed := registry.updateArrayLocked()
			Expect(reconstructed).To(BeTrue())
			Expect(len(arr)).To(Equal(1))
			Expect(registry.values.Load()).NotTo(BeNil())
			Expect(*registry.values.Load()).To(Equal(arr))

			// A subsequent call returns the cached snapshot.
			arr, reconstructed = registry.updateArrayLocked()
			Expect(reconstructed).To(BeFalse())
			Expect(len(arr)).To(Equal(1))
			Expect(registry.values.Load()).NotTo(BeNil())
			Expect(*registry.values.Load()).To(Equal(arr))
		})

		It("should keep the array returned by Array() unchanged after later updates", func() {
			item, item2, item3 := testKeys[0], testKeys[1], testKeys[2]
			registry.Store(item, item)
			registry.Store(item2, item2)

			// Take a snapshot, then invalidate the cache and rebuild it
			snapshot := registry.Array()
			Expect(snapshot).To(ConsistOf(item, item2))

			registry.Delete(item)
			registry.Store(item3, item3)
			Expect(registry.Array()).To(ConsistOf(item2, item3))

			// The snapshot handed out earlier must not be rewritten
			Expect(snapshot).To(ConsistOf(item, item2))
		})

		It("should keep Array() and Len() consistent while registry is updated concurrently", func() {
			// Store the keys first, so Array()/Len() take the cached path. The
			// writer below only re-stores existing keys, the registry therefore
			// holds exactly len(testKeys) elements during the whole test.
			for _, item := range testKeys {
				registry.Store(item, item)
			}
			Expect(registry.Array()).To(HaveLen(len(testKeys)))

			const reads = 50000
			started, stopped := make(chan struct{}), make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				close(started)
				for i := 0; ; i++ {
					select {
					case <-stopped:
						return
					default:
					}
					item := testKeys[i%len(testKeys)]
					registry.Store(item, item)
				}
			}()
			<-started

			corrupted := 0
			for i := 0; i < reads; i++ {
				if len(registry.Array()) != len(testKeys) || registry.Len() != len(testKeys) {
					corrupted++
				}
			}
			close(stopped)
			wg.Wait()

			Expect(corrupted).To(Equal(0))
			Expect(registry.Array()).To(ConsistOf(testKeys[0], testKeys[1], testKeys[2]))
		})
	})

	Context("CustomizedRegisty", func() {
		var registry *CustomizedRegistry[*v1.Pod, *PodArray]

		BeforeEach(func() {
			registry = NewRegistryWithArrayProvider[*v1.Pod, *PodArray](func(arr []*v1.Pod) *PodArray {
				return &PodArray{Pods: arr}
			})
		})

		It("should nil customized registry status accessible", func() {
			var registry *CustomizedRegistry[*v1.Pod, *PodArray]
			Expect(registry.Len()).To(Equal(0))
			Expect(registry.Array().Len()).To(Equal(0))
			Expect(registry.Array()).To(BeNil())
		})

		It("verify empty customized registry status", func() {
			Expect(registry.Len()).To(Equal(0))
			Expect(registry.Array().Len()).To(Equal(0))
			Expect(registry.Array()).ToNot(BeNil())
			Expect(registry.Array().Pods).To(BeEmpty())

			Expect(func() { registry.Delete("testItem") }).NotTo(Panic())
		})

		It("should reflect customized registry updates after added new element", func() {
			// Add an item to the registry
			key, item := testKeys[0], &v1.Pod{}
			registry.Store(key, item)

			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array().Len()).To(Equal(1))
			Expect(len(registry.Array().Pods)).To(Equal(1))
			Expect(registry.Array().Pods).To(ContainElement(item))

			// Remove the item from the registry
			key2, item2 := testKeys[1], &v1.Pod{}
			registry.Store(key2, item2)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(2))
			Expect(registry.Array().Len()).To(Equal(2))
			Expect(len(registry.Array().Pods)).To(Equal(2))
			Expect(registry.Array().Pods).To(ContainElement(item))
			Expect(registry.Array().Pods).To(ContainElement(item2))
		})

		It("should reflect customized registry updates after element changes", func() {
			// Add an item to the registry
			key, item := testKeys[0], &v1.Pod{}
			registry.Store(key, item)

			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array().Len()).To(Equal(1))
			Expect(len(registry.Array().Pods)).To(Equal(1))
			Expect(registry.Array().Pods).To(ContainElement(item))

			// Remove the item from the registry
			item2 := &v1.Pod{}
			registry.Store(key, item2)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array().Len()).To(Equal(1))
			Expect(len(registry.Array().Pods)).To(Equal(1))
			Expect(registry.Array().Pods).To(ContainElement(item2))
		})

		It("should reflect customized registry updates after removal element", func() {
			// Add an item to the registry
			key, item := testKeys[0], &v1.Pod{}
			registry.Store(key, item)
			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array().Len()).To(Equal(1))
			Expect(len(registry.Array().Pods)).To(Equal(1))
			Expect(registry.Array().Pods).To(ContainElement(item))

			// Remove the item from the registry
			registry.Delete(key)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(0))
			Expect(registry.Array().Len()).To(Equal(0))
			Expect(len(registry.Array().Pods)).To(Equal(0))
			Expect(registry.Array().Pods).NotTo(ContainElement(item))

			// Check 0 length leads same result as nil
			registry.Store(key, item)
			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array().Len()).To(Equal(1))
			Expect(len(registry.Array().Pods)).To(Equal(1))
			Expect(registry.Array().Pods).To(ContainElement(item))
		})

		It("should keep the array returned by customized Array() unchanged after later updates", func() {
			// Name the pods, so the entries of a snapshot can be told apart
			key, item := testKeys[0], testPod(testKeys[0])
			key2, item2 := testKeys[1], testPod(testKeys[1])
			registry.Store(key, item)
			registry.Store(key2, item2)

			// Take a snapshot, then invalidate the cache and rebuild it
			snapshot := registry.Array()
			Expect(snapshot.Pods).To(ConsistOf(item, item2))

			key3, item3 := testKeys[2], testPod(testKeys[2])
			registry.Delete(key)
			registry.Store(key3, item3)
			Expect(registry.Array().Pods).To(ConsistOf(item2, item3))

			// The snapshot handed out earlier must not be rewritten
			Expect(snapshot.Pods).To(ConsistOf(item, item2))
		})

		It("should keep customized Array() consistent while registry is updated concurrently", func() {
			// Store the keys first, so Array() takes the cached path. The writer
			// below only re-stores existing keys, the registry therefore holds
			// exactly len(testKeys) elements during the whole test.
			for _, key := range testKeys {
				registry.Store(key, &v1.Pod{})
			}
			Expect(registry.Array().Len()).To(Equal(len(testKeys)))

			const reads = 50000
			started, stopped := make(chan struct{}), make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				close(started)
				for i := 0; ; i++ {
					select {
					case <-stopped:
						return
					default:
					}
					registry.Store(testKeys[i%len(testKeys)], &v1.Pod{})
				}
			}()
			<-started

			corrupted := 0
			for i := 0; i < reads; i++ {
				arr := registry.Array()
				if arr == nil || arr.Len() != len(testKeys) {
					corrupted++
					continue
				}
				for _, pod := range arr.All() {
					if pod == nil {
						corrupted++
					}
				}
			}
			close(stopped)
			wg.Wait()

			Expect(corrupted).To(Equal(0))
			Expect(registry.Array().Len()).To(Equal(len(testKeys)))
		})
	})
})
