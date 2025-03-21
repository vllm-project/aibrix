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

package cache

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

var (
	testKeys = []string{"key1", "key2", "key3"}
)

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
			Expect(registry.Array()).To(ContainElement(item))

			// Remove the item from the registry
			item2 := testKeys[1]
			registry.Store(item, item2)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array()).To(ContainElement(item2))
		})

		It("should reflect registry updates after removal element", func() {
			// Add an item to the registry
			item := testKeys[0]
			registry.Store(item, item)
			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array()).To(ContainElement(item))

			// Remove the item from the registry
			registry.Delete(item)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(0))
			Expect(registry.Array()).NotTo(ContainElement(item))

			// Check 0 length leads same result as nil
			item2 := testKeys[1]
			registry.Store(item, item)
			registry.Store(item2, item2)
			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(2))
			Expect(registry.Array()).To(ContainElement(item))
			Expect(registry.Array()).To(ContainElement(item2))
		})
	})

	Context("CustomizedRegisty", func() {
		var registry *CustomizedRegistry[*v1.Pod, *utils.PodArray]

		BeforeEach(func() {
			registry = NewRegistryWithArrayProvider[*v1.Pod, *utils.PodArray](func(arr []*v1.Pod) *utils.PodArray {
				return &utils.PodArray{Pods: arr}
			})
		})

		It("should nil customized registry status accessible", func() {
			var registry *CustomizedRegistry[*v1.Pod, *utils.PodArray]
			Expect(registry.Len()).To(Equal(0))
			Expect(registry.Array()).To(BeNil())
		})

		It("verify empty customized registry status", func() {
			Expect(registry.Len()).To(Equal(0))
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
			Expect(registry.Array().Pods).To(ContainElement(item))

			// Remove the item from the registry
			key2, item2 := testKeys[1], &v1.Pod{}
			registry.Store(key2, item2)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(2))
			Expect(registry.Array().Pods).To(ContainElement(item))
			Expect(registry.Array().Pods).To(ContainElement(item2))
		})

		It("should reflect customized registry updates after element changes", func() {
			// Add an item to the registry
			key, item := testKeys[0], &v1.Pod{}
			registry.Store(key, item)

			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array().Pods).To(ContainElement(item))

			// Remove the item from the registry
			item2 := &v1.Pod{}
			registry.Store(key, item2)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array().Pods).To(ContainElement(item2))
		})

		It("should reflect customized registry updates after removal element", func() {
			// Add an item to the registry
			key, item := testKeys[0], &v1.Pod{}
			registry.Store(key, item)
			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array().Pods).To(ContainElement(item))

			// Remove the item from the registry
			registry.Delete(key)
			// Check if the item is no longer in the Array() output
			Expect(registry.Len()).To(Equal(0))
			Expect(registry.Array().Pods).NotTo(ContainElement(item))

			// Check 0 length leads same result as nil
			registry.Store(key, item)
			// Check if the item is in the Array() output
			Expect(registry.Len()).To(Equal(1))
			Expect(registry.Array().Pods).To(ContainElement(item))
		})
	})
})
