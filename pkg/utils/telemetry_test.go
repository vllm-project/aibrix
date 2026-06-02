/*
Copyright 2024 The Aibrix Team.

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
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestOTELEnabled(t *testing.T) {
	tests := []struct {
		name        string
		tracesEnv   string
		endpointEnv string
		expected    bool
	}{
		{
			name:        "Both empty should return false",
			tracesEnv:   "",
			endpointEnv: "",
			expected:    false,
		},
		{
			name:        "Only Traces endpoint set should return true",
			tracesEnv:   "http://localhost:4318",
			endpointEnv: "",
			expected:    true,
		},
		{
			name:        "Only General endpoint set should return true",
			tracesEnv:   "",
			endpointEnv: "http://localhost:4317",
			expected:    true,
		},
		{
			name:        "Both endpoints set should return true",
			tracesEnv:   "http://localhost:4318",
			endpointEnv: "http://localhost:4317",
			expected:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// use t.Setenv recover env
			if tt.tracesEnv != "" {
				t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", tt.tracesEnv)
			} else {
				_ = os.Unsetenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
			}

			if tt.endpointEnv != "" {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tt.endpointEnv)
			} else {
				_ = os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
			}

			actual := OTELEnabled()
			if actual != tt.expected {
				t.Errorf("OTELEnabled() = %v, want %v", actual, tt.expected)
			}
		})
	}
}

func TestNewExporter(t *testing.T) {
	tests := []struct {
		name      string
		protocol  string
		expectErr bool
	}{
		{
			name:      "Valid HTTP protocol",
			protocol:  "http",
			expectErr: false,
		},
		{
			name:      "Valid GRPC protocol",
			protocol:  "grpc",
			expectErr: false,
		},
		{
			name:      "Unsupported protocol",
			protocol:  "unknown",
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			exporter, err := newExporter(ctx, tt.protocol)

			if tt.expectErr {
				if err == nil {
					t.Errorf("newExporter(%q) expected error, got nil", tt.protocol)
				}
				if exporter != nil {
					t.Errorf("newExporter(%q) expected nil exporter when error occurs", tt.protocol)
				}
			} else {
				if err != nil {
					t.Errorf("newExporter(%q) unexpected error: %v", tt.protocol, err)
				}
				if exporter == nil {
					t.Errorf("newExporter(%q) returned nil exporter", tt.protocol)
				}
			}
		})
	}
}

func TestInitOpenTelemetry(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		protocol    string
		expectErr   bool
	}{
		{
			name:        "Success initialization with GRPC",
			serviceName: "test-service",
			protocol:    "grpc",
			expectErr:   false,
		},
		{
			name:        "Success initialization with HTTP",
			serviceName: "test-service-http",
			protocol:    "http",
			expectErr:   false,
		},
		{
			name:        "Failure due to invalid protocol",
			serviceName: "test-service",
			protocol:    "invalid-protocol",
			expectErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalProvider := otel.GetTracerProvider()
			originalPropagator := otel.GetTextMapPropagator()
			defer func() {
				otel.SetTracerProvider(originalProvider)
				otel.SetTextMapPropagator(originalPropagator)
			}()

			telemetryData, err := InitOpenTelemetry(tt.serviceName, tt.protocol)

			if tt.expectErr {
				if err == nil {
					t.Errorf("InitOpenTelemetry() expected error, got nil")
				}
				if telemetryData != nil {
					t.Errorf("InitOpenTelemetry() expected nil result on error")
				}
			} else {
				if err != nil {
					t.Errorf("InitOpenTelemetry() unexpected error: %v", err)
				}
				if telemetryData == nil {
					t.Errorf("InitOpenTelemetry() returned nil Telemetry")
				}
				if telemetryData != nil && telemetryData.tracerProvider == nil {
					t.Errorf("InitOpenTelemetry() returned Telemetry with nil tracerProvider")
				}

				currentPropagator := otel.GetTextMapPropagator()
				fields := currentPropagator.Fields()

				hasTraceParent := false
				hasBaggage := false

				for _, f := range fields {
					if f == "traceparent" {
						hasTraceParent = true
					}
					if f == "baggage" {
						hasBaggage = true
					}
				}

				if !hasTraceParent || !hasBaggage {
					t.Errorf("Expected composite propagator with 'traceparent' and 'baggage', but got fields: %v", fields)
				}
			}
		})
	}
}

func TestTelemetry_Shutdown(t *testing.T) {
	t.Run("Shutdown with nil tracerProvider", func(t *testing.T) {
		telemetry := &Telemetry{
			tracerProvider: nil,
		}
		// shouldn't panic
		telemetry.Shutdown()
	})

	t.Run("Shutdown with initialized tracerProvider", func(t *testing.T) {
		tp := sdktrace.NewTracerProvider()
		telemetry := &Telemetry{
			tracerProvider: tp,
		}
		// shouldn't panic or error
		telemetry.Shutdown()
	})
}
