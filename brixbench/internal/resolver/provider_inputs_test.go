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
