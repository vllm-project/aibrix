package resolver

import (
	"strings"
	"testing"
)

func TestValidateAIBrixSourceSelectionAcceptsSingleSourceInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		test Test
	}{
		{
			name: "version",
			test: Test{Name: "aibrix", Version: "v0.6.0"},
		},
		{
			name: "commit",
			test: Test{Name: "aibrix", Commit: "deadbeef"},
		},
		{
			name: "localPath",
			test: Test{Name: "aibrix", LocalPath: "~/aibrix"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAIBrixSourceSelection(&tc.test); err != nil {
				t.Fatalf("validateAIBrixSourceSelection returned error: %v", err)
			}
		})
	}
}

func TestValidateAIBrixSourceSelectionRejectsControlPlaneSourceInput(t *testing.T) {
	test := Test{Name: "aibrix", ControlPlane: []string{"dependency.yaml", "core.yaml"}}

	err := validateAIBrixSourceSelection(&test)
	if err == nil {
		t.Fatalf("expected controlplane source input error")
	}
	if !strings.Contains(err.Error(), "does not support controlplane") {
		t.Fatalf("expected controlplane unsupported error, got %v", err)
	}
}

func TestValidateAIBrixSourceSelectionRejectsMissingSourceInput(t *testing.T) {
	test := Test{Name: "aibrix"}

	err := validateAIBrixSourceSelection(&test)
	if err == nil {
		t.Fatalf("expected missing source input error")
	}
	if !strings.Contains(err.Error(), "requires exactly one source input") {
		t.Fatalf("expected source input error, got %v", err)
	}
}

func TestValidateAIBrixSourceSelectionRejectsMultipleSourceInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		test Test
		want string
	}{
		{
			name: "version and commit",
			test: Test{Name: "aibrix", Version: "v0.6.0", Commit: "deadbeef"},
			want: "version, commit",
		},
		{
			name: "version and localPath",
			test: Test{Name: "aibrix", Version: "v0.6.0", LocalPath: "~/aibrix"},
			want: "version, localPath",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAIBrixSourceSelection(&tc.test)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), "mutually exclusive") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected mutually exclusive error containing %q, got %v", tc.want, err)
			}
		})
	}
}
