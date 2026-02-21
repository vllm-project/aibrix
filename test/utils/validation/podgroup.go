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

package validation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ValidatePodGroupCRDExist(ctx context.Context, dc dynamic.Interface, podGroup client.Object) {
	gomega.Eventually(func() error {
		crd := schema.GroupVersionResource{
			Group:    "apiextensions.k8s.io",
			Version:  "v1",
			Resource: "customresourcedefinitions",
		}
		crdName := fmt.Sprintf("%ss.%s", strings.ToLower(podGroup.GetObjectKind().GroupVersionKind().Kind),
			podGroup.GetObjectKind().GroupVersionKind().Group)
		if _, err := dc.Resource(crd).Get(ctx, crdName, metav1.GetOptions{}); err != nil {
			if apierrors.IsNotFound(err) { // crd is not installed
				return fmt.Errorf("%s %s not found", crdName, podGroup.GetObjectKind().GroupVersionKind().Kind)
			}
			return err
		}
		return nil
	}, time.Second*10, time.Millisecond*250).Should(
		gomega.Succeed(), "PodGroup crd validation failed")
}

func ValidatePodGroupSpec(ctx context.Context, dc dynamic.Interface, podGroup client.Object,
	minMember int32, namespace, name string) {
	gomega.Eventually(func() error {
		var pg *unstructured.Unstructured
		var err error
		if pg, err = dc.Resource(podGroup.GetObjectKind().GroupVersionKind().GroupVersion().WithResource("podgroups")).
			Namespace(namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("failed to get latest podGroup: %w", err)
		}
		value, found, err := unstructured.NestedInt64(pg.Object, "spec", "minMember")
		if err != nil {
			return fmt.Errorf("failed to get minMember: %w", err)
		}
		if !found {
			return fmt.Errorf("minMember not found by name %s and namespace %s", name, namespace)
		}
		if value != int64(minMember) {
			return fmt.Errorf("podgroup minMember is %d, but want %d", value, minMember)
		}

		return nil
	}, time.Second*10, time.Millisecond*250).Should(
		gomega.Succeed(), "PodGroup spec validation failed")
}

func ValidatePodGroupLabels(ctx context.Context, dc dynamic.Interface, podGroup client.Object,
	labels map[string]string, ns, name string) {
	gomega.Eventually(func() error {
		var pg *unstructured.Unstructured
		var err error
		if pg, err = dc.Resource(podGroup.GetObjectKind().GroupVersionKind().GroupVersion().WithResource("podgroups")).
			Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("failed to get latest podGroup: %w", err)
		}
		list, found, err := unstructured.NestedStringMap(pg.Object, "metadata", "labels")
		if err != nil {
			return fmt.Errorf("failed to get status: %w", err)
		}
		if !found {
			return fmt.Errorf("status not found by name %s and namespace %s", name, ns)
		}
		for key, value := range labels {
			if list[key] != value {
				return fmt.Errorf("podGroup label %s is %s, but want %s", key, value, labels[key])
			}
		}

		return nil
	}, time.Second*10, time.Millisecond*250).Should(
		gomega.Succeed(), "PodGroup labels validation failed")
}

func ValidatePodGroupCount(ctx context.Context, dc dynamic.Interface, podGroup client.Object,
	ns string, expectedCount int) {
	gomega.Eventually(func() error {
		var pg *unstructured.UnstructuredList
		var err error
		if pg, err = dc.Resource(podGroup.GetObjectKind().GroupVersionKind().GroupVersion().WithResource("podgroups")).
			Namespace(ns).List(ctx, metav1.ListOptions{}); err != nil {
			return fmt.Errorf("failed to get podGroup list: %w", err)
		}
		if len(pg.Items) != expectedCount {
			return fmt.Errorf("podGroup count is %d, but want %d", len(pg.Items), expectedCount)
		}
		return nil
	}, time.Second*10, time.Millisecond*250).Should(
		gomega.Succeed(), "PodGroup lists validation failed")
}
