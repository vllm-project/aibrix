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

package utils

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"google.golang.org/grpc/credentials"
	"k8s.io/klog/v2"
)

type Telemetry struct {
	tracerProvider *sdktrace.TracerProvider
}

func (t *Telemetry) Shutdown() {
	if t == nil || t.tracerProvider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := t.tracerProvider.Shutdown(ctx); err != nil {
		klog.ErrorS(err, "Error stopping OpenTelemetry TracerProvider")
	} else {
		klog.InfoS("OpenTelemetry telemetry data successfully flushed")
	}
}

func OTELEnabled() bool {
	otlpTraceEndpoint := LoadEnv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	otlpEndpoint := LoadEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	return otlpTraceEndpoint != "" || otlpEndpoint != ""
}

func InitOpenTelemetry(serviceName string, protocol string) (*Telemetry, error) {
	ctx := context.Background()

	exporter, err := newExporter(ctx, protocol)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Telemetry{
		tracerProvider: tp,
	}, nil
}

func newExporter(ctx context.Context, protocol string) (sdktrace.SpanExporter, error) {
	switch protocol {
	case "http":
		var opts []otlptracehttp.Option
		if LoadEnvBool("OTEL_EXPORTER_OTLP_INSECURE_SKIP_VERIFY", false) {
			tlsCfg := &tls.Config{
				InsecureSkipVerify: true,
			}
			opts = append(opts, otlptracehttp.WithTLSClientConfig(tlsCfg))
		}
		return otlptracehttp.New(ctx, opts...)
	case "grpc":
		var opts []otlptracegrpc.Option
		if LoadEnvBool("OTEL_EXPORTER_OTLP_INSECURE_SKIP_VERIFY", false) {
			tlsCfg := &tls.Config{
				InsecureSkipVerify: true,
			}
			opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsCfg)))
		}
		return otlptracegrpc.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unsupported OTLP protocol: %s", protocol)
	}
}
