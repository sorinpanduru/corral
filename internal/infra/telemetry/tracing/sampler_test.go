package tracing_test

import (
	"context"
	"fmt"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
)

// Expected API (plan T11):
//   tracing.NewSampler(cfg tracing.EnvConfig, ratio float64) sdktrace.Sampler
// The tests always pass ratio == cfg.SampleRatio, as T15 will.

var (
	// Low 8 bytes zero: below any non-zero ratio threshold, so a ratio sampler
	// with ratio > 0 samples it.
	lowTraceID = trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0, 0, 0, 0, 0, 0, 0, 0}
	// Low bytes all 0xff: above any ratio < 1, so a ratio sampler with
	// ratio < 1 drops it.
	highTraceID = trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
)

func cfgWith(kind tracing.SamplerKind, ratio float64) tracing.EnvConfig {
	return tracing.EnvConfig{Sampler: kind, SampleRatio: ratio}
}

// decide runs the sampler for a span with the given trace ID and parent
// (parentFlags < 0 means no parent, i.e. a root span).
func decide(s sdktrace.Sampler, id trace.TraceID, parent parentKind) bool {
	ctx := context.Background()
	if parent != noParent {
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    id,
			SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
			TraceFlags: parent.flags(),
			Remote:     parent == remoteSampled || parent == remoteUnsampled,
		})
		ctx = trace.ContextWithSpanContext(ctx, sc)
	}
	r := s.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: ctx,
		TraceID:       id,
		Name:          "synthetic",
		Kind:          trace.SpanKindServer,
	})
	return r.Decision == sdktrace.RecordAndSample
}

type parentKind int

const (
	noParent parentKind = iota
	localSampled
	localUnsampled
	remoteSampled
	remoteUnsampled
)

func (p parentKind) flags() trace.TraceFlags {
	if p == localSampled || p == remoteSampled {
		return trace.FlagsSampled
	}
	return 0
}

var allParents = []parentKind{localSampled, localUnsampled, remoteSampled, remoteUnsampled}

func (p parentKind) String() string {
	return [...]string{"root", "local-sampled", "local-unsampled", "remote-sampled", "remote-unsampled"}[p]
}

// FR-21, AC-16: the default sampler is parent-based trace-ID-ratio; with
// ratio 1.0 all root spans are sampled, with 0.0 none are.
func TestSamplerDefaultRatioBoundaries(t *testing.T) {
	for _, id := range []trace.TraceID{lowTraceID, highTraceID} {
		s1 := tracing.NewSampler(cfgWith(tracing.SamplerUnset, 1.0), 1.0)
		if !decide(s1, id, noParent) {
			t.Errorf("ratio 1.0 must sample every root span (trace %s)", id)
		}
		s0 := tracing.NewSampler(cfgWith(tracing.SamplerUnset, 0.0), 0.0)
		if decide(s0, id, noParent) {
			t.Errorf("ratio 0.0 must sample no root span (trace %s)", id)
		}
	}
}

// FR-21: an intermediate ratio is applied to roots by trace ID.
func TestSamplerDefaultIntermediateRatio(t *testing.T) {
	s := tracing.NewSampler(cfgWith(tracing.SamplerUnset, 0.5), 0.5)
	if !decide(s, lowTraceID, noParent) {
		t.Error("ratio 0.5: low trace ID root must be sampled")
	}
	if decide(s, highTraceID, noParent) {
		t.Error("ratio 0.5: high trace ID root must not be sampled")
	}
}

// FR-21, AC-25: the default sampler is parent-based, so children inherit the
// parent's decision whatever the ratio, for local and remote parents.
func TestSamplerDefaultChildrenInheritParent(t *testing.T) {
	for _, ratio := range []float64{0.0, 0.5, 1.0} {
		for _, id := range []trace.TraceID{lowTraceID, highTraceID} {
			s := tracing.NewSampler(cfgWith(tracing.SamplerUnset, ratio), ratio)
			for _, p := range allParents {
				want := p == localSampled || p == remoteSampled
				if got := decide(s, id, p); got != want {
					t.Errorf("ratio %v trace %s parent %s: sampled=%v, want %v", ratio, id, p, got, want)
				}
			}
		}
	}
}

// FR-21, AC-16: each of the six OTEL_TRACES_SAMPLER kinds maps to the
// matching sdktrace sampler. Behaviour is checked on roots and on each kind
// of parent, using ratio 0.0 and 1.0 so that the ratio does not hide the kind.
func TestSamplerKindsMapToMatchingSDKSamplers(t *testing.T) {
	// always_on / always_off ignore the parent entirely.
	// parentbased_* follow the parent and use their root sampler for roots.
	cases := []struct {
		kind  tracing.SamplerKind
		ratio float64
		root  bool
		// decision for sampled parent, unsampled parent
		sampledParent, unsampledParent bool
	}{
		{tracing.SamplerAlwaysOn, 0.0, true, true, true},
		{tracing.SamplerAlwaysOff, 1.0, false, false, false},
		{tracing.SamplerTraceIDRatio, 1.0, true, true, true},
		{tracing.SamplerTraceIDRatio, 0.0, false, false, false}, // ignores parent
		{tracing.SamplerParentBasedAlwaysOn, 0.0, true, true, false},
		{tracing.SamplerParentBasedAlwaysOff, 1.0, false, true, false},
		{tracing.SamplerParentBasedTraceIDRatio, 1.0, true, true, false},
		{tracing.SamplerParentBasedTraceIDRatio, 0.0, false, true, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/ratio=%v", c.kind, c.ratio), func(t *testing.T) {
			s := tracing.NewSampler(cfgWith(c.kind, c.ratio), c.ratio)
			if s == nil {
				t.Fatal("NewSampler returned nil")
			}
			for _, id := range []trace.TraceID{lowTraceID, highTraceID} {
				if c.ratio > 0 && c.ratio < 1 {
					continue
				}
				if got := decide(s, id, noParent); got != c.root {
					t.Errorf("root: sampled=%v, want %v", got, c.root)
				}
				for _, p := range []parentKind{localSampled, remoteSampled} {
					if got := decide(s, id, p); got != c.sampledParent {
						t.Errorf("%s: sampled=%v, want %v", p, got, c.sampledParent)
					}
				}
				for _, p := range []parentKind{localUnsampled, remoteUnsampled} {
					if got := decide(s, id, p); got != c.unsampledParent {
						t.Errorf("%s: sampled=%v, want %v", p, got, c.unsampledParent)
					}
				}
			}
		})
	}
}

// FR-21: kinds are distinguished by their SDK description, so that a
// parent-based sampler is not silently substituted for a plain one.
func TestSamplerKindsDescriptions(t *testing.T) {
	want := map[tracing.SamplerKind]string{
		tracing.SamplerAlwaysOn:                sdktrace.AlwaysSample().Description(),
		tracing.SamplerAlwaysOff:               sdktrace.NeverSample().Description(),
		tracing.SamplerTraceIDRatio:            sdktrace.TraceIDRatioBased(0.25).Description(),
		tracing.SamplerParentBasedAlwaysOn:     sdktrace.ParentBased(sdktrace.AlwaysSample()).Description(),
		tracing.SamplerParentBasedAlwaysOff:    sdktrace.ParentBased(sdktrace.NeverSample()).Description(),
		tracing.SamplerParentBasedTraceIDRatio: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.25)).Description(),
		tracing.SamplerUnset:                   sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.25)).Description(),
	}
	for kind, desc := range want {
		s := tracing.NewSampler(cfgWith(kind, 0.25), 0.25)
		if got := s.Description(); got != desc {
			t.Errorf("kind %q: description %q, want %q", kind, got, desc)
		}
	}
}

// FR-21, AC-16: always_on samples everything even with sample_ratio 0.0.
func TestSamplerAlwaysOnOverridesZeroRatio(t *testing.T) {
	s := tracing.NewSampler(cfgWith(tracing.SamplerAlwaysOn, 0.0), 0.0)
	for _, id := range []trace.TraceID{lowTraceID, highTraceID} {
		for _, p := range []parentKind{noParent, localSampled, localUnsampled, remoteUnsampled} {
			if !decide(s, id, p) {
				t.Errorf("always_on must sample (trace %s, %s)", id, p)
			}
		}
	}
}

// FR-21, AC-16: OTEL_TRACES_SAMPLER_ARG=0.0 with sample_ratio=1.0 samples no
// roots. Resolve (T10) puts the arg into SampleRatio; the sampler must use the
// ratio it is given, and SampleRatio, not the unused SamplerArg pointer
// contents, decides.
func TestSamplerArgZeroOverridesSampleRatioOne(t *testing.T) {
	zero := 0.0
	cfg := tracing.EnvConfig{SamplerArg: &zero, SampleRatio: 0.0}
	s := tracing.NewSampler(cfg, cfg.SampleRatio)
	for _, id := range []trace.TraceID{lowTraceID, highTraceID} {
		if decide(s, id, noParent) {
			t.Errorf("effective ratio 0.0 must sample no roots (trace %s)", id)
		}
	}
}

// FR-21, AC-25: with a real SDK tracer provider, children created under a
// sampled parent are sampled and children under an unsampled parent are not,
// whatever the configured ratio and for every parent-based kind.
func TestSamplerChildrenInheritViaTracerProvider(t *testing.T) {
	kinds := []tracing.SamplerKind{
		tracing.SamplerUnset,
		tracing.SamplerParentBasedAlwaysOn,
		tracing.SamplerParentBasedAlwaysOff,
		tracing.SamplerParentBasedTraceIDRatio,
	}
	for _, kind := range kinds {
		for _, ratio := range []float64{0.0, 1.0} {
			t.Run(fmt.Sprintf("%q/%v", kind, ratio), func(t *testing.T) {
				tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(tracing.NewSampler(cfgWith(kind, ratio), ratio)))
				defer func() { _ = tp.Shutdown(context.Background()) }()
				tr := tp.Tracer("synthetic")

				for _, sampled := range []bool{true, false} {
					flags := trace.TraceFlags(0)
					if sampled {
						flags = trace.FlagsSampled
					}
					parent := trace.NewSpanContext(trace.SpanContextConfig{
						TraceID: highTraceID, SpanID: trace.SpanID{9, 9, 9, 9, 9, 9, 9, 9},
						TraceFlags: flags, Remote: true,
					})
					ctx := trace.ContextWithRemoteSpanContext(context.Background(), parent)
					ctx, child := tr.Start(ctx, "child")
					_, grandchild := tr.Start(ctx, "grandchild")
					if got := child.SpanContext().IsSampled(); got != sampled {
						t.Errorf("child sampled=%v under parent sampled=%v", got, sampled)
					}
					if got := grandchild.SpanContext().IsSampled(); got != sampled {
						t.Errorf("grandchild sampled=%v under parent sampled=%v", got, sampled)
					}
					grandchild.End()
					child.End()
				}
			})
		}
	}
}

// FR-21, AC-25: a sampled root's children are sampled, an unsampled root's
// children are not, without any remote parent.
func TestSamplerLocalRootChildrenFollowRoot(t *testing.T) {
	for _, ratio := range []float64{0.0, 1.0} {
		tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(tracing.NewSampler(cfgWith(tracing.SamplerUnset, ratio), ratio)))
		tr := tp.Tracer("synthetic")
		ctx, root := tr.Start(context.Background(), "root")
		_, child := tr.Start(ctx, "child")
		want := ratio == 1.0
		if root.SpanContext().IsSampled() != want || child.SpanContext().IsSampled() != want {
			t.Errorf("ratio %v: root sampled=%v child sampled=%v, want both %v",
				ratio, root.SpanContext().IsSampled(), child.SpanContext().IsSampled(), want)
		}
		child.End()
		root.End()
		_ = tp.Shutdown(context.Background())
	}
}
