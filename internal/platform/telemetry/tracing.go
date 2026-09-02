package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Tracing wires OpenTelemetry tracing for a process.
//
// The flow worth tracing in NEXUS is not a single request — it is webhook, run
// creation, job scheduling, worker execution, deployment, health verification. Those
// steps happen in different processes minutes apart, so the trace context is carried in
// the event envelope rather than in an HTTP header, and stitched back together by the
// correlation id.
type Tracing struct {
	provider *sdktrace.TracerProvider
	Tracer   trace.Tracer
}

// SetupTracing returns a live exporter when an endpoint is configured and a no-op
// otherwise. A missing collector must never stop the control plane from starting, so
// this returns a usable Tracing in every case and only ever reports configuration
// errors.
func SetupTracing(ctx context.Context, serviceName, version, endpoint string, sampleRate float64) (*Tracing, error) {
	if endpoint == "" {
		return &Tracing{Tracer: noop.NewTracerProvider().Tracer(serviceName)}, nil
	}

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		// ParentBased keeps a trace intact: once a run is sampled, every span belonging
		// to it is sampled too, so a sampled trace is never missing its middle.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRate))),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return &Tracing{provider: tp, Tracer: tp.Tracer(serviceName)}, nil
}

// Shutdown flushes pending spans. Called during graceful shutdown so the spans
// describing a shutdown are not themselves lost to it.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t.provider == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return t.provider.Shutdown(ctx)
}

// Start opens a span with the standard NEXUS attributes applied.
func (t *Tracing) Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return t.Tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

// Common attribute constructors, so the same key is never spelled two ways.
func AttrProject(id string) attribute.KeyValue    { return attribute.String("nexus.project_id", id) }
func AttrRun(id string) attribute.KeyValue        { return attribute.String("nexus.run_id", id) }
func AttrJob(id string) attribute.KeyValue        { return attribute.String("nexus.job_id", id) }
func AttrDeployment(id string) attribute.KeyValue { return attribute.String("nexus.deployment_id", id) }
func AttrEnvironment(n string) attribute.KeyValue { return attribute.String("nexus.environment", n) }
func AttrWorker(id string) attribute.KeyValue     { return attribute.String("nexus.worker_id", id) }
func AttrCorrelation(id string) attribute.KeyValue {
	return attribute.String("nexus.correlation_id", id)
}
func AttrStepType(t string) attribute.KeyValue { return attribute.String("nexus.step_type", t) }
func AttrAttempt(n int) attribute.KeyValue     { return attribute.Int("nexus.attempt", n) }
