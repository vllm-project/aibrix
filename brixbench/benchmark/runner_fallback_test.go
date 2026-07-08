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

package benchmark

import (
	"errors"
	"testing"

	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
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

func TestShouldRunStormServicePreflightOnlyForAIBrix(t *testing.T) {
	aibrixProvider := "aibrix"
	dynamoProvider := "dynamo"
	for _, tc := range []struct {
		name string
		test resolver.Test
		want bool
	}{
		{
			name: "aibrix",
			test: resolver.Test{Provider: &aibrixProvider},
			want: true,
		},
		{
			name: "dynamo",
			test: resolver.Test{Provider: &dynamoProvider},
			want: false,
		},
		{
			name: "null provider",
			test: resolver.Test{},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRunStormServicePreflight(tc.test); got != tc.want {
				t.Fatalf("shouldRunStormServicePreflight() = %t, want %t", got, tc.want)
			}
		})
	}
}
