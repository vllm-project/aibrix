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

package patch

import (
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRemoveFinalizerPatch_RemoveLast(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	pod.SetFinalizers([]string{"a"})

	patch := RemoveFinalizerPatch(pod, "a")
	assert.Equal(t, types.JSONPatchType, patch.Type())

	data, err := patch.Data(nil)
	assert.NoError(t, err)

	var arr []map[string]interface{}
	assert.NoError(t, sonic.Unmarshal(data, &arr))
	assert.Len(t, arr, 1)
	assert.Equal(t, "remove", arr[0]["op"])
	assert.Equal(t, "/metadata/finalizers", arr[0]["path"])
}

func TestRemoveFinalizerPatch_Filtered(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	pod.SetFinalizers([]string{"a", "b"})

	patch := RemoveFinalizerPatch(pod, "a")
	data, _ := patch.Data(nil)

	var arr []map[string]interface{}
	_ = sonic.Unmarshal(data, &arr)
	assert.Len(t, arr, 1)
	assert.Equal(t, "add", arr[0]["op"])
	assert.Equal(t, "/metadata/finalizers", arr[0]["path"])
	vals := arr[0]["value"].([]interface{})
	assert.ElementsMatch(t, []interface{}{"b"}, vals)
}

func TestAddFinalizerPatch(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	pod.SetFinalizers([]string{"a"})

	patch := AddFinalizerPatch(pod, "b", "c")
	assert.Equal(t, types.JSONPatchType, patch.Type())

	data, _ := patch.Data(nil)
	var arr []map[string]interface{}
	_ = sonic.Unmarshal(data, &arr)
	assert.Len(t, arr, 1)
	assert.Equal(t, "add", arr[0]["op"])
	vals := arr[0]["value"].([]interface{})
	// order not guaranteed
	assert.ElementsMatch(t, []interface{}{"a", "b", "c"}, vals)
}

// Ensure the helper complies with client.Patch interface at compile‑time.
var _ client.Patch = RemoveFinalizerPatch(&corev1.Pod{})
