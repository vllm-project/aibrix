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

package resolver

import (
	"strings"
	"testing"
)

func TestValidateProviderInputsAcceptsNullProvider(t *testing.T) {
	test := Test{Name: "baseline", Provider: nil}

	if err := validateProviderInputs(&test); err != nil {
		t.Fatalf("validateProviderInputs returned error: %v", err)
	}
}

func TestValidateProviderInputsRejectsNullProviderUnsupportedInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		test Test
	}{
		{
			name: "version",
			test: Test{Name: "baseline", Version: "v0.6.0"},
		},
		{
			name: "commit",
			test: Test{Name: "baseline", Commit: "abcdef0"},
		},
		{
			name: "controlplane",
			test: Test{Name: "baseline", ControlPlane: []string{"controlplane.yaml"}},
		},
		{
			name: "platform values",
			test: Test{Name: "baseline", Platform: Platform{ValuesFile: "platform.yaml"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProviderInputs(&tc.test)
			if err == nil {
				t.Fatalf("expected provider null unsupported input error")
			}
			if !strings.Contains(err.Error(), "provider null does not support version, commit, controlplane, or platform inputs") {
				t.Fatalf("expected provider null unsupported input error, got %v", err)
			}
		})
	}
}

func TestValidateProviderInputsRejectsLLMdProvider(t *testing.T) {
	provider := "llmd"
	test := Test{Name: "llmd", Provider: &provider}

	err := validateProviderInputs(&test)
	if err == nil {
		t.Fatalf("expected llmd not implemented error")
	}
	if !strings.Contains(err.Error(), "provider llmd is not implemented") {
		t.Fatalf("expected llmd not implemented error, got %v", err)
	}
}

func TestValidateProviderInputsRejectsUnknownProvider(t *testing.T) {
	provider := "unknown"
	test := Test{Name: "unknown", Provider: &provider}

	err := validateProviderInputs(&test)
	if err == nil {
		t.Fatalf("expected unknown provider error")
	}
	if !strings.Contains(err.Error(), `unknown provider "unknown"`) {
		t.Fatalf("expected unknown provider error, got %v", err)
	}
}
