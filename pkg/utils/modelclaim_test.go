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

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
)

func podWithModelClaimAnnotations(anns map[string]string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Annotations: anns}}
}

func TestModelClaimsFromPod(t *testing.T) {
	pod := podWithModelClaimAnnotations(map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "qwen":  `{"model":"qwen-0.5b","port":9001}`,
		constants.ModelClaimPodAnnotationPrefix + "llama": `{"model":"llama-1b","port":9002}`,
		"unrelated.aibrix.ai/x":                           "ignore-me",
	})
	got := ModelClaimsFromPod(pod)
	assert.Equal(t, map[string]int{"qwen-0.5b": 9001, "llama-1b": 9002}, got)
}

func TestModelClaimPortForPod(t *testing.T) {
	pod := podWithModelClaimAnnotations(map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "llama": `{"model":"llama-1b","port":9002}`,
	})
	port, ok := ModelClaimPortForPod(pod, "llama-1b")
	assert.True(t, ok)
	assert.Equal(t, 9002, port)

	_, ok = ModelClaimPortForPod(pod, "not-here")
	assert.False(t, ok)
}

func TestModelClaimsFromPodNilEmptyMalformed(t *testing.T) {
	assert.Nil(t, ModelClaimsFromPod(nil))
	assert.Nil(t, ModelClaimsFromPod(&v1.Pod{}))

	bad := podWithModelClaimAnnotations(map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "bad":   "not-json",
		constants.ModelClaimPodAnnotationPrefix + "empty": `{"model":"","port":1}`,
	})
	assert.Nil(t, ModelClaimsFromPod(bad))
}

func TestModelClaimsFromPodKeepsPortZero(t *testing.T) {
	pod := podWithModelClaimAnnotations(map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "m1": `{"model":"served-m1","port":0}`,
	})
	got := ModelClaimsFromPod(pod)
	assert.Equal(t, map[string]int{"served-m1": 0}, got)
}

func TestModelClaimBindingsFromPodIncludesObservedState(t *testing.T) {
	pod := podWithModelClaimAnnotations(map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "m1": `{"model":"served-m1","port":0,"state":"sleeping"}`,
	})

	got := ModelClaimBindingsFromPod(pod)
	assert.Equal(t, ModelClaimBinding{
		Model: "served-m1", Port: 0, State: constants.ModelClaimRoutingStateSleeping,
	}, got["served-m1"])
}

func TestModelClaimBindingsFromPodRejectsInconsistentStateAndPort(t *testing.T) {
	pod := podWithModelClaimAnnotations(map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "sleeping-port": `{"model":"sleeping-port","port":9001,"state":"sleeping"}`,
		constants.ModelClaimPodAnnotationPrefix + "active-zero":   `{"model":"active-zero","port":0,"state":"active"}`,
	})

	assert.Nil(t, ModelClaimBindingsFromPod(pod))
}
