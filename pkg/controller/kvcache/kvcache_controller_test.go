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

package kvcache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

func Test_getKVCacheBackendFromMetadata(t *testing.T) {
	testCases := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		expected    string
	}{
		{
			name: "valid backend annotation - vineyard",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: constants.KVCacheBackendVineyard,
			},
			expected: constants.KVCacheBackendVineyard,
		},
		{
			name: "valid backend annotation - infinistore",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: constants.KVCacheBackendInfinistore,
			},
			expected: constants.KVCacheBackendInfinistore,
		},
		{
			name: "valid backend annotation - hpkv",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: constants.KVCacheBackendHPKV,
			},
			expected: constants.KVCacheBackendHPKV,
		},
		{
			name:        "nil annotations fall back to the default backend",
			annotations: nil,
			expected:    constants.KVCacheBackendDefault,
		},
		{
			name:        "empty annotation map falls back to the default backend",
			annotations: map[string]string{},
			expected:    constants.KVCacheBackendDefault,
		},
		{
			// Webhooks may be disabled, leaving the annotation present but blank.
			name: "blank backend annotation falls back to the default backend",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: "",
			},
			expected: constants.KVCacheBackendDefault,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kv := &orchestrationv1alpha1.KVCache{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tc.annotations,
				},
			}
			result := getKVCacheBackendFromAnnotations(kv)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func podWithLabels(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kv-ns",
			Labels:    labels,
		},
	}
}

func Test_kvCacheRequestsForPod(t *testing.T) {
	testCases := []struct {
		name     string
		labels   map[string]string
		expected []reconcile.Request
	}{
		{
			name:   "identifier label maps to the named KVCache in the same namespace",
			labels: map[string]string{constants.KVCacheLabelKeyIdentifier: "my-kvcache"},
			expected: []reconcile.Request{
				{NamespacedName: types.NamespacedName{Namespace: "kv-ns", Name: "my-kvcache"}},
			},
		},
		{
			// A blank identifier still selects the KVCache name it names, which is
			// empty; the reconciler then treats it as not found.
			name:   "blank identifier label maps to an empty name",
			labels: map[string]string{constants.KVCacheLabelKeyIdentifier: ""},
			expected: []reconcile.Request{
				{NamespacedName: types.NamespacedName{Namespace: "kv-ns", Name: ""}},
			},
		},
		{
			name:     "unrelated labels enqueue nothing",
			labels:   map[string]string{"app": "other"},
			expected: nil,
		},
		{
			name:     "nil labels enqueue nothing",
			labels:   nil,
			expected: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := kvCacheRequestsForPod(context.Background(), podWithLabels("pod", tc.labels))
			require.Equal(t, tc.expected, got)
		})
	}
}

func Test_podWithLabelFilter(t *testing.T) {
	pred := podWithLabelFilter(constants.KVCacheLabelKeyIdentifier)

	labeled := podWithLabels("labeled", map[string]string{
		constants.KVCacheLabelKeyIdentifier: "my-kvcache",
	})
	unlabeled := podWithLabels("unlabeled", map[string]string{"app": "other"})

	// Every event type the controller subscribes to must gate on the label, and
	// updates are judged on the new object so a Pod that gains the label passes.
	assert.True(t, pred.Create(event.CreateEvent{Object: labeled}))
	assert.False(t, pred.Create(event.CreateEvent{Object: unlabeled}))

	assert.True(t, pred.Delete(event.DeleteEvent{Object: labeled}))
	assert.False(t, pred.Delete(event.DeleteEvent{Object: unlabeled}))

	assert.True(t, pred.Generic(event.GenericEvent{Object: labeled}))
	assert.False(t, pred.Generic(event.GenericEvent{Object: unlabeled}))

	assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: labeled}),
		"a Pod that gains the label should pass")
	assert.False(t, pred.Update(event.UpdateEvent{ObjectOld: labeled, ObjectNew: unlabeled}),
		"a Pod that loses the label should be filtered out")
	assert.False(t, pred.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: unlabeled}))
}
