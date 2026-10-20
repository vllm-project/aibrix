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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
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
		Model: "served-m1", Port: 0, State: constants.ModelClaimRoutingStateSleeping, Claim: "m1",
	}, got["served-m1"])
}

func TestModelClaimBindingsFromPodReadsWhetherTheControllerWakes(t *testing.T) {
	pod := podWithModelClaimAnnotations(map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "m1": `{"model":"served-m1","port":0,"state":"sleeping","wakeByRequest":true}`,
		// A wake request is not a binding, and is not read as one.
		constants.ModelClaimWakeAnnotationPrefix + "m1": "2026-10-01T08:00:00Z",
	})

	got := ModelClaimBindingsFromPod(pod)

	assert.Equal(t, map[string]ModelClaimBinding{"served-m1": {
		Model: "served-m1", Port: 0, State: constants.ModelClaimRoutingStateSleeping, Claim: "m1",
		WakeByRequest: true,
	}}, got)
}

func TestModelClaimBindingsFromPodReadsTheReasonTheControllerGives(t *testing.T) {
	pod := podWithModelClaimAnnotations(map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "m1": `{"model":"served-m1","port":0,"state":"sleeping","wakeByRequest":true,"reason":"WaitingForRoom"}`,
	})

	assert.Equal(t, "WaitingForRoom", ModelClaimBindingsFromPod(pod)["served-m1"].Reason)
}

func TestModelClaimBindingsFromPodRejectsInconsistentStateAndPort(t *testing.T) {
	pod := podWithModelClaimAnnotations(map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "sleeping-port": `{"model":"sleeping-port","port":9001,"state":"sleeping"}`,
		constants.ModelClaimPodAnnotationPrefix + "active-zero":   `{"model":"active-zero","port":0,"state":"active"}`,
	})

	assert.Nil(t, ModelClaimBindingsFromPod(pod))
}

func TestModelClaimVLLMParallelism(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]string
		want    int64
		wantErr string
	}{
		{name: "no args is one card", want: 1},
		{name: "tp times pp", args: map[string]string{"--tensor-parallel-size": "2", "--pipeline-parallel-size": "2"}, want: 4},
		{name: "explicit dp of one", args: map[string]string{"--data-parallel-size": "1"}, want: 1},
		{name: "gpu memory utilization", args: map[string]string{"--gpu-memory-utilization": "0.9"}, wantErr: "incompatible with kvcached"},
		{name: "data parallelism", args: map[string]string{"--data-parallel-size": "2"}, wantErr: "unsupported by fixed topology pools"},
		{name: "not a number", args: map[string]string{"--tensor-parallel-size": "two"}, wantErr: "must be a positive integer"},
		{name: "zero", args: map[string]string{"--pipeline-parallel-size": "0"}, wantErr: "must be a positive integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ModelClaimVLLMParallelism(&modelv1alpha1.ModelClaimEngineConfig{Args: tc.args})
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	got, err := ModelClaimVLLMParallelism(nil)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), got)
}

func TestModelClaimDeclaredBytes(t *testing.T) {
	got, err := ModelClaimDeclaredBytes("kvFloor", resource.MustParse("1Gi"))
	assert.NoError(t, err)
	assert.Equal(t, int64(1<<30), got)

	// A decimal fraction of a binary unit is rounded up to whole bytes.
	fractional := resource.MustParse("5.6Gi")
	got, err = ModelClaimDeclaredBytes("maximumFootprint", fractional)
	assert.NoError(t, err)
	assert.Equal(t, fractional.Value(), got)

	for _, refused := range []string{"0", "-1Gi", "2Pi", "30m"} {
		_, err := ModelClaimDeclaredBytes("kvFloor", resource.MustParse(refused))
		assert.Error(t, err, refused)
	}
}

func TestModelClaimPodGPUCount(t *testing.T) {
	spec := &v1.PodSpec{Containers: []v1.Container{
		{Resources: v1.ResourceRequirements{Limits: v1.ResourceList{NVIDIAGPUResourceName: resource.MustParse("2")}}},
		{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{NVIDIAGPUResourceName: resource.MustParse("1")}}},
		{},
	}}
	assert.Equal(t, int64(3), ModelClaimPodGPUCount(spec))
	assert.Equal(t, int64(0), ModelClaimPodGPUCount(nil))
}
