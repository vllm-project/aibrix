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

package encode

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
)

func makeStagePod(name, role string) *v1.Pod {
	labels := map[string]string{}
	if role != "" {
		labels[pd.RoleLabel] = role
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    labels,
		},
	}
}

func TestSplitEncoders(t *testing.T) {
	s1 := makeStagePod("s1", "")
	s2 := makeStagePod("s2", "decode")
	s3 := makeStagePod("s3", "prefill")
	e1 := makeStagePod("e1", Role)
	e2 := makeStagePod("e2", Role)

	tests := []struct {
		name         string
		pods         []*v1.Pod
		wantServing  []*v1.Pod
		wantEncoders []*v1.Pod
	}{
		{
			name:         "nil input",
			pods:         nil,
			wantServing:  nil,
			wantEncoders: nil,
		},
		{
			name:         "empty input",
			pods:         []*v1.Pod{},
			wantServing:  []*v1.Pod{},
			wantEncoders: nil,
		},
		{
			name:         "no encoders returns input slice",
			pods:         []*v1.Pod{s1, s2, s3},
			wantServing:  []*v1.Pod{s1, s2, s3},
			wantEncoders: nil,
		},
		{
			name:         "all encoders",
			pods:         []*v1.Pod{e1, e2},
			wantServing:  nil,
			wantEncoders: []*v1.Pod{e1, e2},
		},
		{
			name:         "encoder first followed by serving",
			pods:         []*v1.Pod{e1, s1, s2},
			wantServing:  []*v1.Pod{s1, s2},
			wantEncoders: []*v1.Pod{e1},
		},
		{
			name:         "serving first followed by encoder",
			pods:         []*v1.Pod{s1, s2, e1},
			wantServing:  []*v1.Pod{s1, s2},
			wantEncoders: []*v1.Pod{e1},
		},
		{
			name:         "interleaved preserves order in both slices",
			pods:         []*v1.Pod{s1, e1, s2, e2, s3},
			wantServing:  []*v1.Pod{s1, s2, s3},
			wantEncoders: []*v1.Pod{e1, e2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := slices.Clone(tt.pods)
			serving, encoders := SplitEncoders(tt.pods)
			assert.Equal(t, tt.wantServing, serving)
			assert.Equal(t, tt.wantEncoders, encoders)
			assert.Equal(t, orig, tt.pods, "input slice must not be modified")
		})
	}
}

func TestSplitEncoders_NoEncodersZeroAllocs(t *testing.T) {
	pods := make([]*v1.Pod, 200)
	for i := range pods {
		pods[i] = makeStagePod(fmt.Sprintf("pod-%03d", i), "")
	}

	var serving, encoders []*v1.Pod
	allocs := testing.AllocsPerRun(100, func() {
		serving, encoders = SplitEncoders(pods)
	})
	require.Nil(t, encoders)
	require.Len(t, serving, len(pods))
	assert.Same(t, &pods[0], &serving[0], "no-encoder path must return the input slice directly")
	assert.Zero(t, allocs, "no-encoder path must not allocate")
}

func TestSplitEncoders_MixedPreallocatesServingOnce(t *testing.T) {
	// With one encoder (either first or mid-slice) among 200 pods, serving must be
	// allocated once instead of growing through repeated slice doublings.
	for _, encoderIdx := range []int{0, 100} {
		t.Run(fmt.Sprintf("encoder_at_%d", encoderIdx), func(t *testing.T) {
			pods := make([]*v1.Pod, 200)
			for i := range pods {
				role := ""
				if i == encoderIdx {
					role = Role
				}
				pods[i] = makeStagePod(fmt.Sprintf("pod-%03d", i), role)
			}
			var serving, encoders []*v1.Pod
			allocs := testing.AllocsPerRun(100, func() {
				serving, encoders = SplitEncoders(pods)
			})
			require.Len(t, serving, 199)
			require.Len(t, encoders, 1)
			// 1 alloc for serving + 1 alloc for the single-element encoders slice.
			assert.LessOrEqual(t, allocs, 2.0, "serving slice should be preallocated in one shot")
		})
	}
}

func BenchmarkSplitEncoders(b *testing.B) {
	for _, n := range []int{50, 500, 2000} {
		noEncoders := make([]*v1.Pod, n)
		for i := range noEncoders {
			noEncoders[i] = makeStagePod(fmt.Sprintf("pod-%04d", i), "")
		}
		b.Run(fmt.Sprintf("N=%d/no_encoders", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s, e := SplitEncoders(noEncoders)
				if len(s) != n || len(e) != 0 {
					b.Fatalf("unexpected split: serving=%d encoders=%d", len(s), len(e))
				}
			}
		})

		mixed := make([]*v1.Pod, n)
		for i := range mixed {
			role := ""
			if i%20 == 0 {
				role = Role
			}
			mixed[i] = makeStagePod(fmt.Sprintf("pod-%04d", i), role)
		}
		b.Run(fmt.Sprintf("N=%d/5pct_encoders", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s, e := SplitEncoders(mixed)
				if len(s)+len(e) != n || len(e) == 0 {
					b.Fatalf("unexpected split: serving=%d encoders=%d", len(s), len(e))
				}
			}
		})
	}
}
