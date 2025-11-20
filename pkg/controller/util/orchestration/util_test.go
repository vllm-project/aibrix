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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComputeHashRoleSetTemplate(t *testing.T) {
	t1 := &orchestrationv1alpha1.RoleSetTemplateSpec{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"a": "b"},
	}}
	t2 := &orchestrationv1alpha1.RoleSetTemplateSpec{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"a": "c"},
	}}
	h1 := ComputeHash(t1, nil)
	h2 := ComputeHash(t2, nil)
	assert.NotEqual(t, h1, h2)

	var c1, c2 int32 = 1, 2
	assert.NotEqual(t, ComputeHash(t1, &c1), ComputeHash(t1, &c2))
}

func TestValidateControllerRef(t *testing.T) {
	ref := &metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "Pod",
		Controller:         boolPtr(true),
		BlockOwnerDeletion: boolPtr(true),
	}
	assert.NoError(t, ValidateControllerRef(ref))

	table := []struct {
		name string
		ref  *metav1.OwnerReference
		err  string
	}{
		{"nil ref", nil, "controllerRef is nil"},
		{"empty version", &metav1.OwnerReference{Kind: "A", Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}, "empty APIVersion"},
		{"empty kind", &metav1.OwnerReference{APIVersion: "v1", Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}, "empty Kind"},
		{"controller false", &metav1.OwnerReference{APIVersion: "v1", Kind: "X", Controller: boolPtr(false), BlockOwnerDeletion: boolPtr(true)}, "not set to true"},
		{"blockOwnerDeletion false", &metav1.OwnerReference{APIVersion: "v1", Kind: "X", Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(false)}, "not set"},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateControllerRef(tc.ref)
			assert.ErrorContains(t, err, tc.err)
		})
	}
}

func TestIsRoleSetReady(t *testing.T) {
	rs := &orchestrationv1alpha1.RoleSet{
		Status: orchestrationv1alpha1.RoleSetStatus{
			Conditions: []orchestrationv1alpha1.Condition{
				{Type: orchestrationv1alpha1.RoleSetReady, Status: corev1.ConditionTrue},
			},
		},
	}
	assert.True(t, IsRoleSetReady(rs))
	rs.Status.Conditions[0].Status = corev1.ConditionFalse
	assert.False(t, IsRoleSetReady(rs))
}

func TestConditionHelpers(t *testing.T) {
	cond := NewCondition("Available", corev1.ConditionTrue, "Ready", "all good")
	assert.Equal(t, "Available", string(cond.Type))
	conds := []orchestrationv1alpha1.Condition{*cond}
	found := GetCondition(conds, "Available")
	assert.NotNil(t, found)
	assert.Equal(t, "Ready", found.Reason)

	filtered := FilterOutCondition(conds, "Available")
	assert.Len(t, filtered, 0)
}

func TestDeepCopyMap(t *testing.T) {
	m := map[string]string{"a": "1"}
	copy := DeepCopyMap(m)
	assert.Equal(t, m, copy)
	copy["a"] = "2"
	assert.NotEqual(t, m["a"], copy["a"])
}

func TestSlowStartBatch_AllSuccess(t *testing.T) {
	count := 5
	successFn := func(i int) error {
		return nil
	}
	succeeded, err := SlowStartBatch(count, 1, successFn)
	assert.NoError(t, err)
	assert.Equal(t, count, succeeded)
}

func TestSlowStartBatch_WithFailure(t *testing.T) {
	count := 5
	failFn := func(i int) error {
		if i == 1 {
			return errors.New("fail")
		}
		return nil
	}
	succeeded, err := SlowStartBatch(count, 1, failFn)
	assert.Error(t, err)
	assert.Less(t, succeeded, count)
}

func TestMinIntHelpers(t *testing.T) {
	assert.Equal(t, int32(3), MinInt32(3, 4))
	assert.Equal(t, int32(-1), MinInt32(-1, 5))
	assert.Equal(t, 2, MinInt(2, 3))
	assert.Equal(t, -5, MinInt(-5, 0))
}

func TestShorten(t *testing.T) {
	assert.Equal(t, "abcdefghijklmnopqrstuvwxyz", Shorten("abcdefghijklmnopqrstuvwxyz", false, false))
	assert.Equal(t, "aijklmnopqrstuvwxyz-1234567890-1234567890-1234567890-1234567890", Shorten("abcdefghijklmnopqrstuvwxyz-1234567890-1234567890-1234567890-1234567890", false, false))
	assert.Equal(t, "anopqrstuvwxyz-1234567890-1234567890-1234567890-1234567890", Shorten("abcdefghijklmnopqrstuvwxyz-1234567890-1234567890-1234567890-1234567890", false, true))
	assert.Equal(t, "abcdefghijklmnopqrstuvwxyz-1234567890-1234567890-1234567890-123", Shorten("abcdefghijklmnopqrstuvwxyz-1234567890-1234567890-1234567890-1234567890", true, false))
	assert.Equal(t, "abcdefghijklmnopqrstuvwxyz-1234567890-1234567890-123456789", Shorten("abcdefghijklmnopqrstuvwxyz-1234567890-1234567890-1234567890-1234567890", true, true))
}

func boolPtr(b bool) *bool { return &b }
