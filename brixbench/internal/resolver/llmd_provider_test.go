package resolver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateLLMdSourceSelection(t *testing.T) {
	valuesFile := filepath.Join(t.TempDir(), "router-values.yaml")
	if err := os.WriteFile(valuesFile, []byte("router: {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write router values: %v", err)
	}
	test := Test{
		Name:         "llmd",
		Version:      "0.8.1",
		ControlPlane: []string{" " + valuesFile + " "},
	}

	if err := validateLLMdSourceSelection(&test); err != nil {
		t.Fatalf("validateLLMdSourceSelection returned error: %v", err)
	}
	if test.Version != "v0.8.1" {
		t.Fatalf("expected normalized version v0.8.1, got %s", test.Version)
	}
	if test.ControlPlane[0] != valuesFile {
		t.Fatalf("expected trimmed controlplane values file %s, got %s", valuesFile, test.ControlPlane[0])
	}
}

func TestValidateLLMdSourceSelectionRejectsMissingVersion(t *testing.T) {
	valuesFile := filepath.Join(t.TempDir(), "router-values.yaml")
	if err := os.WriteFile(valuesFile, []byte("router: {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write router values: %v", err)
	}
	test := Test{Name: "llmd", ControlPlane: []string{valuesFile}}

	err := validateLLMdSourceSelection(&test)
	if err == nil {
		t.Fatalf("expected missing version error")
	}
	if !strings.Contains(err.Error(), "missing LLM-d version") {
		t.Fatalf("expected missing version error, got %v", err)
	}
}

func TestValidateLLMdSourceSelectionRejectsUnsupportedInputs(t *testing.T) {
	valuesFile := filepath.Join(t.TempDir(), "router-values.yaml")
	if err := os.WriteFile(valuesFile, []byte("router: {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write router values: %v", err)
	}
	for _, tc := range []struct {
		name string
		test Test
		want string
	}{
		{
			name: "commit",
			test: Test{Name: "llmd", Version: "v0.8.1", Commit: "deadbeef", ControlPlane: []string{valuesFile}},
			want: "not commit",
		},
		{
			name: "localPath",
			test: Test{Name: "llmd", Version: "v0.8.1", LocalPath: "~/llm-d", ControlPlane: []string{valuesFile}},
			want: "not localPath",
		},
		{
			name: "platform values",
			test: Test{Name: "llmd", Version: "v0.8.1", ControlPlane: []string{valuesFile}, Platform: Platform{ValuesFile: "platform.yaml"}},
			want: "platform.valuesFile is not supported",
		},
		{
			name: "controlplane",
			test: Test{Name: "llmd", Version: "v0.8.1"},
			want: "requires at least one controlplane values file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLLMdSourceSelection(&tc.test)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateLLMdSourceSelectionRejectsInvalidControlPlaneFile(t *testing.T) {
	for _, tc := range []struct {
		name       string
		valuesFile string
		want       string
	}{
		{
			name:       "missing values file",
			valuesFile: filepath.Join(t.TempDir(), "missing.yaml"),
			want:       "controlplane values file not found",
		},
		{
			name:       "empty values file path",
			valuesFile: " ",
			want:       "controlplane values file 0 is empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			test := Test{Name: "llmd", Version: "v0.8.1", ControlPlane: []string{tc.valuesFile}}

			err := validateLLMdSourceSelection(&test)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestNormalizeLLMdVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "adds v prefix", input: "0.8.1", want: "v0.8.1"},
		{name: "keeps v prefix", input: "v0.8.1", want: "v0.8.1"},
		{name: "trims whitespace", input: "  v0.8.1  ", want: "v0.8.1"},
		{name: "rejects prerelease", input: "v0.8.1-rc.1", wantErr: true},
		{name: "rejects empty", input: "  ", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeLLMdVersion(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeLLMdVersion returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}
