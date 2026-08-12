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

package routingalgorithms

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pdPod(name, roleset, role, replicaIndex string) *v1.Pod {
	labels := map[string]string{
		PDRoleSetIdentifier: roleset,
		PDRoleIdentifier:    role,
	}
	if replicaIndex != "" {
		labels[RoleReplicaIndex] = replicaIndex
	}
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestHasEligibleRoleset(t *testing.T) {
	t.Run("complete single roleset", func(t *testing.T) {
		pods := []*v1.Pod{
			pdPod("prefill-0", "rs1", "prefill", "0"),
			pdPod("decode-0", "rs1", "decode", "0"),
			pdPod("decode-1", "rs1", "decode", "1"),
		}
		assert.True(t, HasEligibleRoleset(pods))
	})

	t.Run("partial decode replicas still eligible with one decode", func(t *testing.T) {
		pods := []*v1.Pod{
			pdPod("prefill-rs1", "rs1", "prefill", "0"),
			pdPod("decode-rs1", "rs1", "decode", "0"),
			pdPod("prefill-rs2", "rs2", "prefill", "0"),
			pdPod("decode-rs2-a", "rs2", "decode", "0"),
			pdPod("decode-rs2-b", "rs2", "decode", "1"),
		}
		assert.True(t, HasEligibleRoleset(pods))
		eligible := eligiblePDRolesets(pods)
		assert.Contains(t, eligible, "rs1")
		assert.Contains(t, eligible, "rs2")
	})

	t.Run("only prefill not eligible", func(t *testing.T) {
		pods := []*v1.Pod{pdPod("prefill-only", "rs1", "prefill", "0")}
		assert.False(t, HasEligibleRoleset(pods))
	})

	t.Run("only decode not eligible", func(t *testing.T) {
		pods := []*v1.Pod{pdPod("decode-only", "rs1", "decode", "0")}
		assert.False(t, HasEligibleRoleset(pods))
	})

	t.Run("mixed fleet sizes both eligible with one prefill and one decode", func(t *testing.T) {
		// Mirrors production: 9svsx has more replicas than v8h7g; both should route.
		pods := []*v1.Pod{
			pdPod("9svsx-prefill-0", "vllm-qwen3-8b-tp-roleset-9svsx", "prefill", "0"),
			pdPod("9svsx-prefill-1", "vllm-qwen3-8b-tp-roleset-9svsx", "prefill", "1"),
			pdPod("9svsx-decode-0", "vllm-qwen3-8b-tp-roleset-9svsx", "decode", "0"),
			pdPod("9svsx-decode-1", "vllm-qwen3-8b-tp-roleset-9svsx", "decode", "1"),
			pdPod("9svsx-decode-2", "vllm-qwen3-8b-tp-roleset-9svsx", "decode", "2"),
			pdPod("v8h7g-prefill-0", "vllm-qwen3-8b-tp-roleset-v8h7g", "prefill", "0"),
			pdPod("v8h7g-prefill-1", "vllm-qwen3-8b-tp-roleset-v8h7g", "prefill", "1"),
			pdPod("v8h7g-decode-0", "vllm-qwen3-8b-tp-roleset-v8h7g", "decode", "0"),
			pdPod("v8h7g-decode-1", "vllm-qwen3-8b-tp-roleset-v8h7g", "decode", "1"),
		}
		_, _, statuses, eligibleCount := DescribePDRolesetEligibility(pods)
		assert.Equal(t, 2, eligibleCount)
		assert.Len(t, statuses, 2)
		byRoleset := map[string]bool{}
		for _, s := range statuses {
			byRoleset[s.Roleset] = s.Eligible
		}
		assert.True(t, byRoleset["vllm-qwen3-8b-tp-roleset-9svsx"])
		assert.True(t, byRoleset["vllm-qwen3-8b-tp-roleset-v8h7g"])
	})
}

func TestPrefillPodsFromEligibleRolesets(t *testing.T) {
	pods := []*v1.Pod{
		pdPod("prefill-rs1", "rs1", "prefill", "0"),
		pdPod("decode-rs1", "rs1", "decode", "0"),
		pdPod("prefill-rs2", "rs2", "prefill", "0"),
		pdPod("decode-rs2-a", "rs2", "decode", "0"),
		pdPod("decode-rs2-b", "rs2", "decode", "1"),
	}
	out := prefillPodsFromEligibleRolesets(pods)
	assert.Len(t, out, 2)
	names := []string{out[0].Name, out[1].Name}
	assert.ElementsMatch(t, []string{"prefill-rs1", "prefill-rs2"}, names)
}
