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

package modelclaim

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestVLLMParallelismIncludesPrefillContextParallelism(t *testing.T) {
	tests := []struct {
		name string
		args map[string]string
		want int64
	}{
		{name: "defaults", want: 1},
		{
			name: "tensor pipeline and prefill context",
			args: map[string]string{
				"--tensor-parallel-size":          "2",
				"--pipeline-parallel-size":        "2",
				"--prefill-context-parallel-size": "2",
			},
			want: 8,
		},
		{
			name: "decode context reuses existing ranks",
			args: map[string]string{
				"--tensor-parallel-size":         "4",
				"--decode-context-parallel-size": "2",
			},
			want: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := vllmParallelism(&modelv1alpha1.ModelClaimEngineConfig{Args: tt.args})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestVLLMParallelismRejectsInvalidContextParallelism(t *testing.T) {
	for _, name := range []string{
		"--prefill-context-parallel-size",
		"--decode-context-parallel-size",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := vllmParallelism(&modelv1alpha1.ModelClaimEngineConfig{
				Args: map[string]string{name: "0"},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), name+" must be a positive integer")
		})
	}
}

func TestVLLMParallelismRejectsPrefillContextOverflow(t *testing.T) {
	_, err := vllmParallelism(&modelv1alpha1.ModelClaimEngineConfig{
		Args: map[string]string{
			"--tensor-parallel-size":          strconv.FormatInt(math.MaxInt64, 10),
			"--prefill-context-parallel-size": "2",
		},
	})

	require.Error(t, err)
	assert.Equal(t, "tensor, pipeline, and prefill context parallelism product overflows int64", err.Error())
}

func TestVLLMParallelismRejectsTensorPipelineOverflow(t *testing.T) {
	_, err := vllmParallelism(&modelv1alpha1.ModelClaimEngineConfig{
		Args: map[string]string{
			"--tensor-parallel-size":   strconv.FormatInt(math.MaxInt64, 10),
			"--pipeline-parallel-size": "2",
		},
	})

	require.Error(t, err)
	assert.Equal(t, "tensor and pipeline parallelism product overflows int64", err.Error())
}
