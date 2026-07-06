package resolver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDynamoSourceSelection(t *testing.T) {
	valuesFile := filepath.Join(t.TempDir(), "platform-values.yaml")
	if err := os.WriteFile(valuesFile, []byte("nats: {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write platform values: %v", err)
	}
	test := Test{
		Name:     "dynamo",
		Version:  "1.2.1",
		Platform: Platform{ValuesFile: " " + valuesFile + " "},
	}

	if err := validateDynamoSourceSelection(&test); err != nil {
		t.Fatalf("validateDynamoSourceSelection returned error: %v", err)
	}
	if test.Version != "v1.2.1" {
		t.Fatalf("expected normalized version v1.2.1, got %s", test.Version)
	}
	if test.Platform.ValuesFile != valuesFile {
		t.Fatalf("expected trimmed platform values file %s, got %s", valuesFile, test.Platform.ValuesFile)
	}
}

func TestValidateDynamoSourceSelectionRejectsMissingVersion(t *testing.T) {
	test := Test{
		Name: "dynamo",
	}

	err := validateDynamoSourceSelection(&test)
	if err == nil {
		t.Fatalf("expected missing version error")
	}
	if !strings.Contains(err.Error(), "missing Dynamo version") {
		t.Fatalf("expected missing version error, got %v", err)
	}
}

func TestValidateDynamoSourceSelectionRejectsUnsupportedInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		test Test
		want string
	}{
		{
			name: "commit",
			test: Test{Name: "dynamo", Version: "v1.2.1", Commit: "deadbeef"},
			want: "not commit",
		},
		{
			name: "localPath",
			test: Test{Name: "dynamo", Version: "v1.2.1", LocalPath: "~/dynamo"},
			want: "not localPath",
		},
		{
			name: "controlplane",
			test: Test{Name: "dynamo", Version: "v1.2.1", ControlPlane: []string{"platform.yaml"}},
			want: "controlplane is not supported",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDynamoSourceSelection(&tc.test)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateDynamoSourceSelectionRejectsNonStableVersion(t *testing.T) {
	for _, version := range []string{
		"v1.2.0-deepseek-v4-dev.3",
		"v1.1.0-rc0",
		"v0.7.0.post1",
	} {
		t.Run(version, func(t *testing.T) {
			test := Test{Name: "dynamo", Version: version}

			err := validateDynamoSourceSelection(&test)
			if err == nil {
				t.Fatalf("expected error for version %s", version)
			}
			if !strings.Contains(err.Error(), "expected stable semver") {
				t.Fatalf("expected stable semver error, got %v", err)
			}
		})
	}
}

func TestValidateDynamoSourceSelectionRejectsInvalidPlatformValuesFile(t *testing.T) {
	for _, tc := range []struct {
		name       string
		valuesFile string
		want       string
	}{
		{
			name:       "missing values file",
			valuesFile: filepath.Join(t.TempDir(), "missing.yaml"),
			want:       "platform.valuesFile not found",
		},
		{
			name:       "reference path",
			valuesFile: filepath.Join(".tmp", "dynamo-reference", "platform-values.yaml"),
			want:       "must not reference .tmp/dynamo-reference",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			test := Test{
				Name:     "dynamo",
				Version:  "v1.2.1",
				Platform: Platform{ValuesFile: tc.valuesFile},
			}

			err := validateDynamoSourceSelection(&test)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestNormalizeDynamoVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "adds v prefix",
			input: "1.2.1",
			want:  "v1.2.1",
		},
		{
			name:  "keeps v prefix",
			input: "v1.2.1",
			want:  "v1.2.1",
		},
		{
			name:  "trims whitespace",
			input: "  v1.2.1  ",
			want:  "v1.2.1",
		},
		{
			name:    "rejects prerelease",
			input:   "v1.2.0-deepseek-v4-dev.3",
			wantErr: true,
		},
		{
			name:    "rejects rc",
			input:   "v1.1.0-rc0",
			wantErr: true,
		},
		{
			name:    "rejects post",
			input:   "v0.7.0.post1",
			wantErr: true,
		},
		{
			name:    "rejects empty",
			input:   "  ",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeDynamoVersion(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeDynamoVersion returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}
