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

package constants

import "testing"

func TestModelNameFromMetadata(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		want        string
		wantOK      bool
	}{
		{
			name: "label takes precedence",
			labels: map[string]string{
				ModelLabelName: "label-model",
			},
			annotations: map[string]string{
				ModelLabelName: "annotation-model",
			},
			want:   "label-model",
			wantOK: true,
		},
		{
			name: "annotation supports path-style model name",
			annotations: map[string]string{
				ModelLabelName: "/models/mock",
			},
			want:   "/models/mock",
			wantOK: true,
		},
		{
			name: "empty metadata has no model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ModelNameFromMetadata(tt.labels, tt.annotations)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("ModelNameFromMetadata() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
