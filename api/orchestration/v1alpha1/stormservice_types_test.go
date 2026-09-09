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

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"
)

func TestStormServiceSpecResolvedMode(t *testing.T) {
	tests := map[string]struct {
		replicas *int32
		mode     StormServiceMode
		want     StormServiceMode
	}{
		"unset replicas infer pooled":      {replicas: nil, want: StormServicePooledMode},
		"replicas 1 infers pooled":         {replicas: ptr.To[int32](1), want: StormServicePooledMode},
		"replicas 3 infers replica":        {replicas: ptr.To[int32](3), want: StormServiceReplicaMode},
		"explicit mode wins over replicas": {replicas: ptr.To[int32](1), mode: StormServiceReplicaMode, want: StormServiceReplicaMode},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			spec := &StormServiceSpec{Replicas: tc.replicas, Mode: tc.mode}
			assert.Equal(t, tc.want, spec.ResolvedMode())
		})
	}
}

func TestStormServiceSpecResolvedReplicas(t *testing.T) {
	tests := map[string]struct {
		replicas *int32
		want     int32
	}{
		"unset resolves to the documented default of 1": {replicas: nil, want: 1},
		"explicit zero is preserved":                    {replicas: ptr.To[int32](0), want: 0},
		"explicit count is returned as is":              {replicas: ptr.To[int32](3), want: 3},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			spec := &StormServiceSpec{Replicas: tc.replicas}
			assert.Equal(t, tc.want, spec.ResolvedReplicas())
		})
	}
}
