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

package cache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/types"
)

// profileCache serves one fixed profile for every pod.
type profileCache struct {
	Cache
	profile *ModelGPUProfile
}

func (c profileCache) GetModelProfileByPod(*v1.Pod, string) (*ModelGPUProfile, error) {
	return c.profile, nil
}

type fixedOutputPredictor struct{ out int }

func (f fixedOutputPredictor) Predict(int) int            { return f.out }
func (f fixedOutputPredictor) AddTrace(_, _ int, _ int32) {}

func TestPendingLoadProviderGetConsumptionReportsProfileErrors(t *testing.T) {
	indexes := [][]float64{{0, 1}, {0, 1}}
	tputs := [][]float64{{10, 10}, {10, 10}}

	tests := []struct {
		name        string
		profile     *ModelGPUProfile
		wantErr     error
		wantNilErr  bool
		wantConsume float64
	}{
		{
			name:    "no latency data",
			profile: &ModelGPUProfile{Indexes: indexes, Tputs: tputs},
			wantErr: ErrProfileNoE2E,
		},
		{
			name: "latency table too small for the signature",
			// One row of one value, while the throughput table is 2x2: the signature of a
			// request lands outside it.
			profile: &ModelGPUProfile{Indexes: indexes, Tputs: tputs, E2E: [][]float64{{0.5}}},
		},
		{
			name:        "complete profile",
			profile:     &ModelGPUProfile{Indexes: indexes, Tputs: tputs, E2E: [][]float64{{0.5, 0.5}, {0.5, 0.5}}},
			wantNilErr:  true,
			wantConsume: 1.0 / 10.0 / 0.5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newPendingLoadProvider(profileCache{profile: tt.profile})
			ctx := types.NewRoutingContext(context.Background(), "test", "model", "hello world", "req-1", "")
			ctx.SetOutputPredictor(fixedOutputPredictor{out: 2})

			consumption, err := provider.GetConsumption(ctx, &v1.Pod{})
			if tt.wantNilErr {
				assert.NoError(t, err)
				assert.InDelta(t, tt.wantConsume, consumption, 1e-9)
				return
			}
			// A profile that cannot give a latency must be reported, not treated as a
			// request that consumes nothing.
			assert.Error(t, err, "got consumption=%v with no error", consumption)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			}
		})
	}
}
