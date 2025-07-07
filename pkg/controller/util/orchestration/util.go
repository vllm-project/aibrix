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
	"sync"

	hashutil "github.com/vllm-project/aibrix/pkg/utils/hash"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
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

	return rand.SafeEncodeString(fmt.Sprint(roleSetTemplateSpecHasher.Sum32()))
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
