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
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"
	"sync"

	hashutil "github.com/vllm-project/aibrix/pkg/utils/hash"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/integer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// ComputeHash returns a hash value calculated from stormService template and
// a collisionCount to avoid hash collision. The hash will be safe encoded to
// avoid bad words.
func ComputeHash(template *orchestrationv1alpha1.RoleSetTemplateSpec, collisionCount *int32) string {
	roleSetTemplateSpecHasher := fnv.New32a()
	hashutil.DeepHashObject(roleSetTemplateSpecHasher, *template)

	// Add collisionCount in the hash if it exists.
	if collisionCount != nil {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(*collisionCount))
		roleSetTemplateSpecHasher.Write(collisionCountBytes)
	}

	return hashutil.ShortSafeEncodeString(roleSetTemplateSpecHasher.Sum32())
}

func ValidateControllerRef(controllerRef *metav1.OwnerReference) error {
	if controllerRef == nil {
		return fmt.Errorf("controllerRef is nil")
	}
	if len(controllerRef.APIVersion) == 0 {
		return fmt.Errorf("controllerRef has empty APIVersion")
	}
	if len(controllerRef.Kind) == 0 {
		return fmt.Errorf("controllerRef has empty Kind")
	}
	if controllerRef.Controller == nil || !*controllerRef.Controller {
		return fmt.Errorf("controllerRef.Controller is not set to true")
	}
	if controllerRef.BlockOwnerDeletion == nil || !*controllerRef.BlockOwnerDeletion {
		return fmt.Errorf("controllerRef.BlockOwnerDeletion is not set")
	}
	return nil
}

func IsRoleSetReady(rs *orchestrationv1alpha1.RoleSet) bool {
	if rs == nil {
		return false
	}
	for _, c := range rs.Status.Conditions {
		if c.Type == orchestrationv1alpha1.RoleSetReady && c.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

// NewCondition creates a new stormService condition.
func NewCondition(condType orchestrationv1alpha1.ConditionType, status v1.ConditionStatus, reason, message string) *orchestrationv1alpha1.Condition {
	now := metav1.Now()
	return &orchestrationv1alpha1.Condition{
		Type:               condType,
		Status:             status,
		LastUpdateTime:     &now,
		LastTransitionTime: &now,
		Reason:             reason,
		Message:            message,
	}
}

// GetCondition returns the condition with the provided type.
func GetCondition(conditions orchestrationv1alpha1.Conditions, condType orchestrationv1alpha1.ConditionType) *orchestrationv1alpha1.Condition {
	for i := range conditions {
		c := conditions[i]
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

// FilterOutCondition returns a new slice of stormService conditions without conditions with the provided type.
func FilterOutCondition(conditions []orchestrationv1alpha1.Condition, condType orchestrationv1alpha1.ConditionType) []orchestrationv1alpha1.Condition {
	var newConditions []orchestrationv1alpha1.Condition
	for _, c := range conditions {
		if c.Type == condType {
			continue
		}
		newConditions = append(newConditions, c)
	}
	return newConditions
}

func DeepCopyMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}

	dest := make(map[string]string)
	for k, v := range source {
		dest[k] = v
	}

	return dest
}

func SlowStartBatch(count int, initialBatchSize int, fn func(int) error) (int, error) {
	remaining := count
	successes := 0
	finished := 0
	for batchSize := integer.IntMin(remaining, initialBatchSize); batchSize > 0; batchSize = integer.IntMin(2*batchSize, remaining) {
		errCh := make(chan error, batchSize)
		var wg sync.WaitGroup
		wg.Add(batchSize)
		for i := 0; i < batchSize; i++ {
			go func(index int) {
				defer func() {
					err := recover()
					if err != nil {
						klog.Errorf("slowStartBatch panic: %v", err)
					}
				}()
				defer wg.Done()
				if err := fn(index); err != nil {
					errCh <- err
				}
			}(finished + i)
		}
		wg.Wait()
		curSuccesses := batchSize - len(errCh)
		successes += curSuccesses
		if len(errCh) > 0 {
			return successes, <-errCh
		}
		remaining -= batchSize
		finished += batchSize
	}
	return successes, nil
}

func UpdateStatus(ctx context.Context, scheme *runtime.Scheme, cli client.Client, obj client.Object) error {
	firstTry := true
	meta := obj
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if firstTry {
			firstTry = false
			if err := cli.Status().Update(ctx, obj); err != nil {
				return err
			}
		} else {
			// TODO: consider efficiency
			freshRaw, err := scheme.New(obj.GetObjectKind().GroupVersionKind())
			if err != nil {
				return err
			}

			fresh, ok := freshRaw.(client.Object)
			if !ok {
				return fmt.Errorf("new object is not a client.Object: %T", freshRaw)
			}

			if err := cli.Get(ctx, types.NamespacedName{Name: meta.GetName(), Namespace: meta.GetNamespace()}, fresh); err != nil {
				return err
			}

			freshObj, ok := fresh.(metav1.Object)
			if !ok {
				return fmt.Errorf("fresh is not a metav1.Object")
			}
			meta.SetResourceVersion(freshObj.GetResourceVersion())
			if err := cli.Status().Update(ctx, obj); err != nil {
				return err
			}
		}
		return nil
	})
}

func resetObject(obj runtime.Object) error {
	t := reflect.TypeOf(obj)
	if t.Kind() != reflect.Ptr {
		return fmt.Errorf("runtime object types must be pointers to structs, but got %s", t.Kind())
	}

	newed := reflect.New(t.Elem())
	reflect.ValueOf(obj).Elem().Set(newed.Elem())
	return nil
}

// Patch object with retry.
// Original object would change.
func Patch(ctx context.Context, cli client.Client, obj client.Object, patch client.Patch) error {
	firstTry := true
	meta, ok := obj.(metav1.Object)
	if !ok {
		return fmt.Errorf("object types must be metav1.Object, but got %T", obj)
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if firstTry {
			firstTry = false
			if err := cli.Patch(ctx, obj, patch); err != nil {
				return err
			}
		} else {
			if err := resetObject(obj); err != nil {
				return err
			}

			if err := cli.Get(ctx, types.NamespacedName{Name: meta.GetName(), Namespace: meta.GetNamespace()}, obj); err != nil {
				return err
			}

			if err := cli.Patch(ctx, obj, patch); err != nil {
				return err
			}
		}
		return nil
	})
}

func MinInt32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func MinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func EnsurePodGroup(ctx context.Context, dc dynamic.Interface, podGroup client.Object, name, namespace string) (synced bool, err error) {
	crd := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
	crdName := fmt.Sprintf("%ss.%s", strings.ToLower(podGroup.GetObjectKind().GroupVersionKind().Kind), podGroup.GetObjectKind().GroupVersionKind().Group)
	if _, err := dc.Resource(crd).Get(ctx, crdName, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) { // crd is not installed
			return false, nil
		}
		return false, err
	}

	if res, err := dc.Resource(podGroup.GetObjectKind().GroupVersionKind().GroupVersion().WithResource("podgroups")).Namespace(namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) { // podgroup not found, create it
			podGroupUnstructed, err := runtime.DefaultUnstructuredConverter.ToUnstructured(podGroup)
			if err != nil {
				return false, err
			}
			_, err = dc.Resource(podGroup.GetObjectKind().GroupVersionKind().GroupVersion().WithResource("podgroups")).Namespace(namespace).Create(ctx, &unstructured.Unstructured{Object: podGroupUnstructed}, metav1.CreateOptions{})
			return err == nil, err
		}
		return false, err // other get error
	} else {
		// podgroup already presented, update pod group if needed.
		podGroup.SetResourceVersion(res.GetResourceVersion())
		podGroupUnstructed, err := runtime.DefaultUnstructuredConverter.ToUnstructured(podGroup)
		if err != nil {
			return false, err
		}

		// Only update if the spec has changed.
		if reflect.DeepEqual(res.Object["spec"], podGroupUnstructed["spec"]) {
			return false, nil
		}

		_, err = dc.Resource(podGroup.GetObjectKind().GroupVersionKind().GroupVersion().WithResource("podgroups")).Namespace(namespace).Update(ctx, &unstructured.Unstructured{Object: podGroupUnstructed}, metav1.UpdateOptions{})
		return err == nil, err
	}
}

func FinalizePodGroup(ctx context.Context, dc dynamic.Interface, c client.Client, podGroup client.Object, name, namespace string) error {
	crd := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
	if _, err := dc.Resource(crd).Get(ctx, fmt.Sprintf("%ss.%s", strings.ToLower(podGroup.GetObjectKind().GroupVersionKind().Kind), podGroup.GetObjectKind().GroupVersionKind().Group), metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) { // crd is not installed
			return nil
		}
		return err
	}

	if _, err := dc.Resource(podGroup.GetObjectKind().GroupVersionKind().GroupVersion().WithResource("podgroups")).Namespace(namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) { // podgroup not found, done
			return nil
		}
		return err
	}
	return dc.Resource(podGroup.GetObjectKind().GroupVersionKind().GroupVersion().WithResource("podgroups")).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// keepPrefix controls the truncation strategy:
// - When keepPrefix is true, keep the prefix (the leading part of the name).
// - When keepPrefix is false, keep the first character plus the trailing part of the name.
func Shorten(name string, keepPrefix, isGenerateName bool) string {
	maxLength := 63
	if isGenerateName {
		maxLength = 58 // 63 - 5 // 5 char for generated.
	}
	if len(name) < maxLength {
		return name
	}

	if keepPrefix {
		return name[:maxLength]
	}

	// keep the hash part to prevent name conflicts
	// keep the first letter to ensure the truncated name starts with a valid character
	return string(name[0]) + name[len(name)-maxLength+1:]
}
