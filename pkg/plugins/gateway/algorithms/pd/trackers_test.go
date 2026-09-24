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

package pd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRequestTrackers_SameNameInTwoNamespaces: the request trackers are shared
// by every model the router serves, so same-named pods in two namespaces must
// be counted separately.
func TestRequestTrackers_SameNameInTwoNamespaces(t *testing.T) {
	podA := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-0", Namespace: "team-a"}}
	podB := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-0", Namespace: "team-b"}}
	keyA := utils.GeneratePodKey(podA.Namespace, podA.Name)
	keyB := utils.GeneratePodKey(podB.Namespace, podB.Name)

	prefill := NewPrefillRequestTracker()
	prefill.AddPrefillRequest("req-1", keyA)
	prefill.AddPrefillRequest("req-2", keyA)
	assert.Equal(t, 2, prefill.GetPrefillRequestCountsForPod(keyA))
	assert.Equal(t, 0, prefill.GetPrefillRequestCountsForPod(keyB))
	assert.Equal(t, map[string]int32{"prefill-0": 2}, prefill.GetPrefillRequestCountsForPods([]*v1.Pod{podA}))
	assert.Equal(t, map[string]int32{"prefill-0": 0}, prefill.GetPrefillRequestCountsForPods([]*v1.Pod{podB}))
	prefill.RemovePrefillRequest("req-1")
	assert.Equal(t, 1, prefill.GetPrefillRequestCountsForPod(keyA))

	pending := NewPendingDecodeTracker()
	pending.AddPendingDecode("req-1", keyA)
	assert.Equal(t, float64(1), pending.GetPendingDecodeCount(keyA))
	assert.Equal(t, float64(0), pending.GetPendingDecodeCount(keyB))
	pending.RemovePendingDecode("req-1")
	assert.Equal(t, float64(0), pending.GetPendingDecodeCount(keyA))
}
