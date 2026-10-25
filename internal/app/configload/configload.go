// Package configload is the SPEC-003 loader contract as one application-layer
// function: it runs the loader over the CORRAL settings, then validates the
// OTEL_* variables when the effective tracing.enabled is known, and returns
// one Outcome with the error groups in FR-13 order.
//
// Compliance (SPEC-003 FR-11, FR-25, GDPR data minimisation): all inputs are
// injected, nothing here reads a process global or logs. Errors carry setting
// names and constant sentinels only.
package configload

import (
	"errors"

	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
)

// ErrRegistry reports a registry that lacks the settings the tracing
// validation needs (a programming error, never input).
var ErrRegistry = errors.New("configload: registry lacks the tracing settings")

// ErrNoReader reports a nil file reader (a programming error).
var ErrNoReader = errors.New("configload: no file reader")

// Inputs are the injected inputs (FR-25).
type Inputs struct {
	Args     []string
	Env      []string // KEY=VALUE entries
	ReadFile source.ReadFile
}

// Outcome is the result of Load. Errors holds groups 1 to 4 in FR-13 order:
// command line, per-input, cross rules, then OTEL_*. Observability is
// meaningful only when Errors is empty; TracingEnabled, Tracing and TLSIgnored
// are set only on an outcome without errors (tracing.Resolve returns a zero
// result on any error).
type Outcome struct {
	Mode           loader.Mode
	Result         loader.Result
	Observability  settings.Observability
	TracingEnabled bool
	Tracing        tracing.EnvConfig
	TLSIgnored     []string
	Errors         []error
	// Err is a programming error (a registry without the tracing settings, a
	// nil file reader). It is never a *config.Error and never appears in
	// Errors; the runtime maps it to exit code 1, not to FR-11 records.
	Err error
}

// Load selects the mode and, for run and check-config, loads and validates
// everything a run would. Help and version return at once, before any
// environment or file access.
func Load(r *registry.Registry, in Inputs) Outcome {
	mode := loader.SelectMode(in.Args)
	if mode == loader.ModeHelp || mode == loader.ModeVersion {
		return Outcome{Mode: mode}
	}
	_, okE := r.Lookup(settings.TracingEnabled)
	ratioDecl, okR := r.Lookup(settings.TracingSampleRatio)
	if !okE || !okR {
		return Outcome{Mode: mode, Err: ErrRegistry}
	}
	read := in.ReadFile
	if read == nil {
		return Outcome{Mode: mode, Err: ErrNoReader}
	}

	env := source.NewEnv(in.Env)
	res := loader.Load(r, loader.Inputs{Args: in.Args, Env: env, ReadFile: read})
	out := Outcome{Mode: mode, Result: res, Observability: settings.ObservabilityFrom(res)}
	out.Errors = append(out.Errors, res.Errors...)

	// OTEL_* is validated only when the effective tracing.enabled is known
	// (FR-19); a failed tracing.sample_ratio falls back to its default.
	if res.Failed(settings.TracingEnabled) {
		return out
	}
	enabled, _ := res.Get(settings.TracingEnabled)
	on, _ := enabled.(bool)
	ratio, _ := ratioDecl.DefaultValue.(float64)
	if v, ok := res.Get(settings.TracingSampleRatio); ok {
		if f, ok := v.(float64); ok {
			ratio = f
		}
	}
	traceOn, cfg, errs := tracing.Resolve(tracing.ResolveInputs{Enabled: on, SampleRatio: ratio}, env.Lookup, read)
	out.Errors = append(out.Errors, errs...)
	if len(out.Errors) == 0 {
		out.TracingEnabled, out.Tracing = traceOn, cfg
		if traceOn {
			out.TLSIgnored = tracing.TLSIgnoredSettings(cfg, env.Lookup)
		}
	}
	return out
}
