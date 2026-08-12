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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestModelRouterName(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		want      string
	}{
		{name: "preserves existing convention", modelName: "llama2-7b", want: "llama2-7b-router"},
		{name: "supports path model name", modelName: "/models/mock"},
		{name: "supports punctuation", modelName: "///"},
		{name: "limits long names", modelName: strings.Repeat("model/", 100)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := ModelRouterName(tt.modelName)
			second := ModelRouterName(tt.modelName)
			if first != second {
				t.Fatalf("route name is not deterministic: %q, %q", first, second)
			}
			if tt.want != "" && first != tt.want {
				t.Fatalf("route name = %q, want %q", first, tt.want)
			}
			if errs := validation.IsDNS1123Subdomain(first); len(errs) > 0 {
				t.Fatalf("route name %q is invalid: %v", first, errs)
			}
		})
	}
}
