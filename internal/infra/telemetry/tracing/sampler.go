package tracing

import (
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// NewSampler maps the validated OTEL_TRACES_SAMPLER kind to an SDK sampler
// (FR-21). SamplerUnset gives the default parent-based trace-ID-ratio sampler.
// ratio is the effective ratio (OTEL_TRACES_SAMPLER_ARG or tracing.sample_ratio,
// already validated to [0,1] by Resolve and config); it is used by ratio-based
// kinds and ignored by the others.
func NewSampler(cfg EnvConfig, ratio float64) sdktrace.Sampler {
	switch cfg.Sampler {
	case SamplerAlwaysOn:
		return sdktrace.AlwaysSample()
	case SamplerAlwaysOff:
		return sdktrace.NeverSample()
	case SamplerTraceIDRatio:
		return sdktrace.TraceIDRatioBased(ratio)
	case SamplerParentBasedAlwaysOn:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case SamplerParentBasedAlwaysOff:
		return sdktrace.ParentBased(sdktrace.NeverSample())
	case SamplerParentBasedTraceIDRatio, SamplerUnset:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	default:
		// Unreachable for a config produced by Resolve; fall back to default.
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}
