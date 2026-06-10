package benchmark

import (
	"errors"
	"testing"
)

func TestFallbackGatewayEndpointMissing(t *testing.T) {
	t.Setenv("BENCHMARK_GATEWAY_ENDPOINT", "")

	if got, ok := fallbackGatewayEndpoint(); got != "" || ok {
		t.Fatalf("fallbackGatewayEndpoint() = (%q, %t), want (\"\", false)", got, ok)
	}
}

func TestFallbackGatewayEndpointOverride(t *testing.T) {
	const override = "http://127.0.0.1:18080"
	t.Setenv("BENCHMARK_GATEWAY_ENDPOINT", override)

	if got, ok := fallbackGatewayEndpoint(); got != override || !ok {
		t.Fatalf("fallbackGatewayEndpoint() = (%q, %t), want (%q, true)", got, ok, override)
	}
}

func TestResolveGatewayEndpointUsesDetectedEndpoint(t *testing.T) {
	const detected = "http://10.0.0.10:80"
	t.Setenv("BENCHMARK_GATEWAY_ENDPOINT", "http://127.0.0.1:18080")

	got, err := resolveGatewayEndpoint(detected, nil)
	if err != nil {
		t.Fatalf("resolveGatewayEndpoint() returned error: %v", err)
	}
	if got != detected {
		t.Fatalf("resolveGatewayEndpoint() = %q, want %q", got, detected)
	}
}

func TestResolveGatewayEndpointUsesOverrideOnDetectionFailure(t *testing.T) {
	const override = "http://127.0.0.1:18080"
	t.Setenv("BENCHMARK_GATEWAY_ENDPOINT", override)

	got, err := resolveGatewayEndpoint("", errors.New("lookup failed"))
	if err != nil {
		t.Fatalf("resolveGatewayEndpoint() returned error: %v", err)
	}
	if got != override {
		t.Fatalf("resolveGatewayEndpoint() = %q, want %q", got, override)
	}
}

func TestResolveGatewayEndpointFailsWithoutDetectedEndpointOrOverride(t *testing.T) {
	t.Setenv("BENCHMARK_GATEWAY_ENDPOINT", "")

	_, err := resolveGatewayEndpoint("", errors.New("lookup failed"))
	if err == nil {
		t.Fatal("resolveGatewayEndpoint() expected error, got nil")
	}
}
