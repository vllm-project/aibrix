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

package types

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
)

func TestTargetAddressUsesModelClaimPort(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), "algo", "qwen-0.5b", "msg", "r1", "")
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p1",
			Annotations: map[string]string{
				constants.ModelClaimPodAnnotationPrefix + "qwen": `{"model":"qwen-0.5b","port":9001}`,
				constants.ModelClaimPodAnnotationPrefix + "llm":  `{"model":"llama-1b","port":9002}`,
			},
		},
		Status: v1.PodStatus{PodIP: "10.0.0.9"},
	}
	ctx.SetTargetPod(pod)
	assert.Equal(t, "10.0.0.9:9001", ctx.TargetAddress())

	ctx2 := NewRoutingContext(context.Background(), "algo", "llama-1b", "msg", "r2", "")
	ctx2.SetTargetPod(pod)
	assert.Equal(t, "10.0.0.9:9002", ctx2.TargetAddress())
}

func TestTargetAddressFallsBackToLabel(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), "algo", "other-model", "msg", "r1", "")
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "p1",
			Labels: map[string]string{constants.ModelLabelPort: "8000"},
			Annotations: map[string]string{
				constants.ModelClaimPodAnnotationPrefix + "qwen": `{"model":"qwen-0.5b","port":9001}`,
			},
		},
		Status: v1.PodStatus{PodIP: "10.0.0.9"},
	}
	ctx.SetTargetPod(pod)
	assert.Equal(t, "10.0.0.9:8000", ctx.TargetAddress())
}

// A known ModelClaim model held at the non-routable marker (port 0) must not
// fall back to the default deployment port on the warm pool pod.
func TestTargetAddressPortZeroIsNotRoutable(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), "algo", "qwen-0.5b", "msg", "r1", "")
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "p1",
			Labels: map[string]string{constants.ModelLabelPort: "8000"},
			Annotations: map[string]string{
				constants.ModelClaimPodAnnotationPrefix + "qwen": `{"model":"qwen-0.5b","port":0}`,
			},
		},
		Status: v1.PodStatus{PodIP: "10.0.0.9"},
	}
	ctx.SetTargetPod(pod)
	assert.Equal(t, "", ctx.TargetAddress())
}
