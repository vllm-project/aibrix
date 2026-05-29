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
	"k8s.io/klog/v2"
)

type Telemetry struct {
	tracerProvider *sdktrace.TracerProvider
}

func (t *Telemetry) Shutdown() {
	if t.tracerProvider != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := t.tracerProvider.Shutdown(ctx); err != nil {
			klog.ErrorS(err, "Error stopping OpenTelemetry TracerProvider")
		} else {
			klog.InfoS("OpenTelemetry telemetry data successfully flushed")
		}
	}
}

func OTELEnabled() bool {
	otlpTraceEndpoint := LoadEnv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	otlpEndpoint := LoadEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	otlpEnabled := otlpTraceEndpoint != "" || otlpEndpoint != ""
	if otlpEnabled {
		return true
	}
	return false
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
		return otlptracegrpc.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unsupported OTLP protocol: %s", protocol)
	}
}
