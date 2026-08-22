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

package pametrics

import (
	"errors"
	"testing"
)

func TestParseTargetValue(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    float64
		wantErr error
	}{
		"integer":           {value: "50", want: 50},
		"fraction":          {value: "0.5", want: 0.5},
		"quantity":          {value: "100m", wantErr: errInvalidTargetValue},
		"non-numeric":       {value: "abc", wantErr: errInvalidTargetValue},
		"zero":              {value: "0", wantErr: errNonPositiveTargetValue},
		"negative":          {value: "-1", wantErr: errNonPositiveTargetValue},
		"not a number":      {value: "NaN", wantErr: errNonPositiveTargetValue},
		"positive infinity": {value: "+Inf", wantErr: errNonPositiveTargetValue},
		"negative infinity": {value: "-Inf", wantErr: errNonPositiveTargetValue},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseTargetValue(tt.value)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseTargetValue(%q) error=%v, want %v", tt.value, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseTargetValue(%q)=%v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
