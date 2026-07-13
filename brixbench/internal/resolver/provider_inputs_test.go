package resolver

import (
	"os"
	"path/filepath"
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

func TestValidateProviderInputsAcceptsLLMdProvider(t *testing.T) {
	valuesFile := filepath.Join(t.TempDir(), "router-values.yaml")
	if err := os.WriteFile(valuesFile, []byte("router: {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write router values: %v", err)
	}
	provider := "llmd"
	test := Test{Name: "llmd", Provider: &provider, Version: "0.8.1", ControlPlane: []string{" " + valuesFile + " "}}

	if err := validateProviderInputs(&test); err != nil {
		t.Fatalf("validateProviderInputs returned error: %v", err)
	}
	if test.Version != "v0.8.1" {
		t.Fatalf("expected normalized version v0.8.1, got %s", test.Version)
	}
	if test.ControlPlane[0] != valuesFile {
		t.Fatalf("expected trimmed controlplane path %s, got %s", valuesFile, test.ControlPlane[0])
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
