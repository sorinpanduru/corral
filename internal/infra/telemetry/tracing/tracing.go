// Package tracing builds corral's tracer provider (SPEC-001 FR-8, FR-19..FR-21,
// FR-30, FR-33, FR-34, FR-37; ADR-0007).
//
// Compliance (GDPR data minimisation, FR-24): nothing here logs err.Error() or
// the collector endpoint. Errors reach logs only through errclass.LogAttrs and
// constant messages; collector rejections are wrapped in a type whose Error()
// text is constant, so no response body or URL can leak through otel.Handle.
package tracing

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing/spanqueue"
)

// shutdownCap is the FR-34 upper bound for Shutdown.
const shutdownCap = 5 * time.Second

// Options configures Start.
type Options struct {
	Settings config.Settings
	Env      LookupEnv
	Config   EnvConfig
	// Enabled is the result of Resolve.
	Enabled bool
	Metrics *metrics.Factory
	Logger  *slog.Logger
	// Exporter replaces the OTLP exporter. Tests only; ignored when disabled.
	// It is still wrapped by FilterResource (ADR-0007 f).
	Exporter sdktrace.SpanExporter
}

// Tracing owns the tracer provider.
type Tracing struct {
	enabled bool
	tp      trace.TracerProvider
	sdk     *sdktrace.TracerProvider
	logger  *slog.Logger
}

// Start builds the provider. logbridge.Install must have run before (ADR-0006).
func Start(ctx context.Context, o Options) (*Tracing, error) {
	if !o.Enabled {
		return &Tracing{tp: noop.NewTracerProvider()}, nil
	}
	res, err := NewResource(ctx, o.Env)
	if err != nil {
		return nil, err
	}
	cfg := o.Config
	sampler := NewSampler(cfg, cfg.SampleRatio)

	exp := o.Exporter
	if exp == nil && cfg.Exporter == ExporterOTLP {
		exp, err = newOTLP(ctx, cfg.Protocol)
		if err != nil {
			return nil, err
		}
	}

	provOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}
	if exp != nil {
		var dropped spanqueue.DropCounter = discardCounter{}
		if o.Metrics != nil {
			c, cerr := o.Metrics.Counter(metrics.NameTracingSpansDropped)
			if cerr != nil {
				_ = exp.Shutdown(ctx)
				return nil, cerr
			}
			dropped = c
		}
		q := spanqueue.New(FilterResource(classifyExporter{exp}, res), spanqueue.Config{
			MaxQueueSize:       cfg.Queue.MaxQueueSize,
			MaxExportBatchSize: cfg.Queue.MaxExportBatchSize,
			ScheduleDelay:      cfg.Queue.ScheduleDelay,
			ExportTimeout:      cfg.Queue.ExportTimeout,
		}, dropped)
		provOpts = append(provOpts, sdktrace.WithSpanProcessor(q))
	}
	tp := sdktrace.NewTracerProvider(provOpts...)
	if exp != nil {
		otel.SetTracerProvider(tp)
	}
	return &Tracing{enabled: true, tp: tp, sdk: tp, logger: o.Logger}, nil
}

func newOTLP(ctx context.Context, p Protocol) (sdktrace.SpanExporter, error) {
	// The exporters read OTEL_EXPORTER_OTLP_* from the process environment.
	if p == ProtocolGRPC {
		return otlptracegrpc.New(ctx)
	}
	return otlptracehttp.New(ctx)
}

type discardCounter struct{}

func (discardCounter) Add(float64) {}

// Enabled reports whether tracing is on (hook for FR-19).
func (t *Tracing) Enabled() bool { return t.enabled }

// TracerProvider returns the provider (no-op when disabled).
func (t *Tracing) TracerProvider() trace.TracerProvider { return t.tp }

// ForceFlush flushes queued spans, honouring ctx.
func (t *Tracing) ForceFlush(ctx context.Context) error {
	if t.sdk == nil {
		return nil
	}
	return t.sdk.ForceFlush(ctx)
}

// Shutdown stops the provider, bounded by ctx and by 5 s (FR-34).
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t.sdk == nil {
		return nil
	}
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > shutdownCap {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownCap)
		defer cancel()
	}
	err := t.sdk.Shutdown(ctx)
	if err != nil && t.logger != nil {
		t.logger.LogAttrs(ctx, slog.LevelWarn, "tracing shutdown failed", errclass.LogAttrs(err)...)
	}
	return err
}

// rejectionError marks an OTLP collector rejection. Its text is constant so the
// response body and URL never reach logs (FR-24); it deliberately has no Unwrap.
type rejectionError struct{}

func (rejectionError) Error() string  { return "collector rejected export" }
func (rejectionError) ProtocolError() {}

var _ errclass.ProtocolError = rejectionError{}

// classifyRejection wraps collector rejections (HTTP non-2xx, gRPC
// non-transport status) and returns other errors unchanged.
func classifyRejection(err error) error {
	if err == nil {
		return nil
	}
	var s interface{ GRPCStatus() *status.Status }
	if errors.As(err, &s) {
		switch s.GRPCStatus().Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			return err
		}
		return rejectionError{}
	}
	// HTTP rejections carry no typed error; they are the errors that no
	// transport classification (network, timeout, TLS, canceled) explains.
	if c, d := errclass.Classify(err); c == errclass.ClassInternal && d == errclass.DetailOther {
		return rejectionError{}
	}
	return err
}

type classifyExporter struct{ sdktrace.SpanExporter }

func (c classifyExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return classifyRejection(c.SpanExporter.ExportSpans(ctx, spans))
}
