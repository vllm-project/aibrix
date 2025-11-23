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

package orchestration

import (
	"context"

	"github.com/stretchr/testify/mock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// MockDynamicInterface mocking dynamic.Interface
type MockDynamicInterface struct {
	mock.Mock
}

func (m *MockDynamicInterface) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	args := m.Called(resource)
	return args.Get(0).(dynamic.NamespaceableResourceInterface)
}

// MockNamespaceableResourceInterface mocking NamespaceableResourceInterface
type MockNamespaceableResourceInterface struct {
	mock.Mock
}

func (m *MockNamespaceableResourceInterface) Namespace(ns string) dynamic.ResourceInterface {
	args := m.Called(ns)
	return args.Get(0).(dynamic.ResourceInterface)
}

func (m *MockNamespaceableResourceInterface) Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, obj, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, obj, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockNamespaceableResourceInterface) Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, obj, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, obj, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockNamespaceableResourceInterface) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	args := m.Called(ctx, obj, options)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockNamespaceableResourceInterface) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	if len(subresources) == 0 {
		args := m.Called(ctx, name, options)
		return args.Error(0)
	}
	args := m.Called(ctx, name, options, subresources)
	return args.Error(0)
}

func (m *MockNamespaceableResourceInterface) DeleteCollection(ctx context.Context, options metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	args := m.Called(ctx, options, listOptions)
	return args.Error(0)
}

func (m *MockNamespaceableResourceInterface) Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, name, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, name, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockNamespaceableResourceInterface) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(*unstructured.UnstructuredList), args.Error(1)
}

func (m *MockNamespaceableResourceInterface) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(watch.Interface), args.Error(1)
}

func (m *MockNamespaceableResourceInterface) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, name, pt, data, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, name, pt, data, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockNamespaceableResourceInterface) Apply(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, name, obj, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, name, obj, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockNamespaceableResourceInterface) ApplyStatus(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	args := m.Called(ctx, name, obj, options)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

type MockResourceInterface struct {
	mock.Mock
}

func (m *MockResourceInterface) Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, obj, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, obj, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockResourceInterface) Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, obj, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, obj, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockResourceInterface) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	args := m.Called(ctx, obj, options)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockResourceInterface) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	if len(subresources) == 0 {
		args := m.Called(ctx, name, options)
		return args.Error(0)
	}
	args := m.Called(ctx, name, options, subresources)
	return args.Error(0)
}

func (m *MockResourceInterface) DeleteCollection(ctx context.Context, options metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	args := m.Called(ctx, options, listOptions)
	return args.Error(0)
}

func (m *MockResourceInterface) Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, name, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, name, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockResourceInterface) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(*unstructured.UnstructuredList), args.Error(1)
}

func (m *MockResourceInterface) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	args := m.Called(ctx, opts)
	return args.Get(0).(watch.Interface), args.Error(1)
}

func (m *MockResourceInterface) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, name, pt, data, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, name, pt, data, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockResourceInterface) Apply(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(subresources) == 0 {
		args := m.Called(ctx, name, obj, options)
		return args.Get(0).(*unstructured.Unstructured), args.Error(1)
	}
	args := m.Called(ctx, name, obj, options, subresources)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}

func (m *MockResourceInterface) ApplyStatus(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	args := m.Called(ctx, name, obj, options)
	return args.Get(0).(*unstructured.Unstructured), args.Error(1)
}
