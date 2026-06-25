package resolver

import (
	"strings"
	"testing"
)

func TestValidateDynamoSourceSelection(t *testing.T) {
	test := Test{
		Name:    "dynamo",
		Version: "1.2.1",
	}

	if err := validateDynamoSourceSelection(&test); err != nil {
		t.Fatalf("validateDynamoSourceSelection returned error: %v", err)
	}
	if test.Version != "v1.2.1" {
		t.Fatalf("expected normalized version v1.2.1, got %s", test.Version)
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
