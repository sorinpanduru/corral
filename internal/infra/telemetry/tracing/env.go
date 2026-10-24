// Package tracing decides whether tracing is on and validates the OTEL_*
// variables corral honours or validates (FR-19, FR-20, FR-21, FR-27, FR-37).
//
// Compliance (FR-24, FR-27, GDPR data minimisation): a rejected value is never
// echoed. Errors are config.Error values naming the variable only, wrapping a
// constant sentinel. Header values, URL userinfo, certificate paths and file
// contents are never copied into an error, a log record or the result.
package tracing

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
)

// LookupEnv looks up an environment variable, like os.LookupEnv.
type LookupEnv func(key string) (string, bool)

// ExporterKind is the validated OTEL_TRACES_EXPORTER value.
type ExporterKind string

// Exporter kinds (FR-20).
const (
	ExporterOTLP ExporterKind = "otlp"
	ExporterNone ExporterKind = "none"
)

// Protocol is the validated OTLP protocol.
type Protocol string

// OTLP protocols (FR-20).
const (
	ProtocolHTTPProtobuf Protocol = "http/protobuf"
	ProtocolGRPC         Protocol = "grpc"
)

// SamplerKind is the validated OTEL_TRACES_SAMPLER value, or SamplerUnset.
type SamplerKind string

// Sampler kinds (FR-21).
const (
	SamplerUnset                   SamplerKind = ""
	SamplerAlwaysOn                SamplerKind = "always_on"
	SamplerAlwaysOff               SamplerKind = "always_off"
	SamplerTraceIDRatio            SamplerKind = "traceidratio"
	SamplerParentBasedAlwaysOn     SamplerKind = "parentbased_always_on"
	SamplerParentBasedAlwaysOff    SamplerKind = "parentbased_always_off"
	SamplerParentBasedTraceIDRatio SamplerKind = "parentbased_traceidratio"
)

// QueueSettings maps the OTEL_BSP_* variables (FR-37).
type QueueSettings struct {
	MaxQueueSize       int
	MaxExportBatchSize int
	ScheduleDelay      time.Duration
	ExportTimeout      time.Duration
}

// EnvConfig is the validated tracing configuration derived from the
// environment. It holds no header values, paths or file contents.
type EnvConfig struct {
	Exporter ExporterKind
	Protocol Protocol
	// Sampler is SamplerUnset for the default parent-based ratio sampler.
	Sampler SamplerKind
	// SamplerArg is the validated OTEL_TRACES_SAMPLER_ARG, nil when unset or
	// ignored because the effective sampler uses no ratio.
	SamplerArg *float64
	// SampleRatio is the effective ratio: SamplerArg if used, else
	// tracing.sample_ratio.
	SampleRatio float64
	Queue       QueueSettings
}

// maxQueueBound caps OTEL_BSP_MAX_QUEUE_SIZE and OTEL_BSP_MAX_EXPORT_BATCH_SIZE.
const maxQueueBound = 1 << 20

// Variable names.
const (
	varSDKDisabled   = "OTEL_SDK_DISABLED"
	varExporter      = "OTEL_TRACES_EXPORTER"
	varSampler       = "OTEL_TRACES_SAMPLER"
	varSamplerArg    = "OTEL_TRACES_SAMPLER_ARG"
	varResourceAttrs = "OTEL_RESOURCE_ATTRIBUTES"
	varBSPQueue      = "OTEL_BSP_MAX_QUEUE_SIZE"
	varBSPBatch      = "OTEL_BSP_MAX_EXPORT_BATCH_SIZE"
	varBSPDelay      = "OTEL_BSP_SCHEDULE_DELAY"
	varBSPTimeout    = "OTEL_BSP_EXPORT_TIMEOUT"
	otlpPrefix       = "OTEL_EXPORTER_OTLP_"
	otlpTracesPrefix = "OTEL_EXPORTER_OTLP_TRACES_"
)

// Constant sentinels wrapped by config.Error; none carries a value.
var (
	errBadExporter    = errors.New("tracing: unsupported exporter")
	errBadSampler     = errors.New("tracing: unsupported sampler")
	errBadSamplerArg  = errors.New("tracing: sampler argument not a ratio in [0,1]")
	errBadProtocol    = errors.New("tracing: unsupported protocol")
	errBadEndpoint    = errors.New("tracing: invalid endpoint")
	errBadHeaders     = errors.New("tracing: invalid header list")
	errBadKVList      = errors.New("tracing: invalid key=value list")
	errBadPair        = errors.New("tracing: client certificate and key do not pair")
	errBadTimeout     = errors.New("tracing: invalid timeout")
	errBadCompression = errors.New("tracing: unsupported compression")
	errBadBool        = errors.New("tracing: not a boolean")
	errBadFile        = errors.New("tracing: unreadable or non-PEM file")
	errBadPositive    = errors.New("tracing: not a positive integer")
	errBatchTooLarge  = errors.New("tracing: batch size exceeds queue size")
)

// ResolveInputs are the CORRAL settings Resolve needs (SPEC-003 FR-19).
type ResolveInputs struct {
	// Enabled is the effective tracing.enabled.
	Enabled bool
	// SampleRatio is the effective tracing.sample_ratio.
	SampleRatio float64
}

// resolver collects per-variable errors and caches PEM reads for one Resolve
// call. Cached bytes are discarded when Resolve returns.
type resolver struct {
	env    LookupEnv
	read   source.ReadFile
	errs   map[string]error
	pem    map[string]pemEntry
	failed map[string]bool
}

type pemEntry struct {
	data []byte
	ok   bool
}

func (r *resolver) get(name string) (string, bool) {
	v, ok := r.env(name)
	v = strings.TrimSpace(v) // whitespace-only counts as unset (FR-7 set)
	return v, ok && v != ""
}

func (r *resolver) fail(name string, cause error) {
	if _, dup := r.errs[name]; !dup {
		r.errs[name] = config.NewError(name, cause)
	}
	r.failed[name] = true
}

// readPEM reads path through the injected reader at most once per call.
func (r *resolver) readPEM(path string) ([]byte, bool) {
	if e, ok := r.pem[path]; ok {
		return e.data, e.ok
	}
	b, err := r.read(path, source.MaxPEMFileBytes)
	e := pemEntry{data: b, ok: err == nil && len(b) > 0}
	r.pem[path] = e
	return e.data, e.ok
}

// Resolve decides whether tracing is on and, when it is, validates the OTEL_*
// variables (FR-19, FR-20, FR-21, FR-27, FR-37). All inputs are injected: env
// for variables and read for PEM files (FR-25).
//
// Tracing is off when in.Enabled is false or OTEL_SDK_DISABLED is "true"
// (case-insensitive). Any other OTEL_SDK_DISABLED value is false and never an
// error. When off, no other variable is read. When on, every honoured or
// validated variable is checked (both the general and _TRACES_ variants) and
// every failure is returned, one config.Error per variable, sorted by setting.
// A check that involves a variable with its own error is skipped (AC-20). An
// invalid exporter does not stop other validation (AC-21). On any error the
// returned enabled flag is false and cfg is zero. Whitespace-only values count
// as unset.
func Resolve(in ResolveInputs, env LookupEnv, read source.ReadFile) (bool, EnvConfig, []error) {
	if v, ok := env(varSDKDisabled); ok && strings.EqualFold(v, "true") {
		return false, EnvConfig{}, nil
	}
	if !in.Enabled {
		return false, EnvConfig{}, nil
	}
	r := &resolver{
		env: env, read: read, errs: map[string]error{}, pem: map[string]pemEntry{},
		failed: map[string]bool{},
	}
	cfg := r.resolve(in)
	if len(r.errs) > 0 {
		out := make([]error, 0, len(r.errs))
		for _, e := range r.errs {
			out = append(out, e)
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].(*config.Error).Setting < out[j].(*config.Error).Setting
		})
		return false, EnvConfig{}, out
	}
	return true, cfg, nil
}

func (r *resolver) resolve(in ResolveInputs) EnvConfig {
	cfg := EnvConfig{
		Exporter:    ExporterOTLP,
		Protocol:    ProtocolHTTPProtobuf,
		SampleRatio: in.SampleRatio,
		Queue: QueueSettings{
			MaxQueueSize:       2048,
			MaxExportBatchSize: 512,
			ScheduleDelay:      5000 * time.Millisecond,
			ExportTimeout:      30000 * time.Millisecond,
		},
	}

	if v, ok := r.get(varExporter); ok {
		switch ExporterKind(strings.ToLower(v)) {
		case ExporterOTLP, ExporterNone:
			cfg.Exporter = ExporterKind(strings.ToLower(v))
		default:
			r.fail(varExporter, errBadExporter)
		}
	}

	// Protocol: the _TRACES_ variant wins, but both are validated.
	for _, name := range []string{otlpPrefix + "PROTOCOL", otlpTracesPrefix + "PROTOCOL"} {
		v, ok := r.get(name)
		if !ok {
			continue
		}
		switch Protocol(strings.ToLower(v)) {
		case ProtocolHTTPProtobuf, ProtocolGRPC:
			cfg.Protocol = Protocol(strings.ToLower(v)) // the later (_TRACES_) variant overrides
		default:
			r.fail(name, errBadProtocol)
		}
	}

	// Remaining exporter variables: both variants validated, nothing kept.
	for _, suffix := range []string{"ENDPOINT", "HEADERS", "TIMEOUT", "COMPRESSION", "INSECURE", "CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY"} {
		for _, name := range []string{otlpPrefix + suffix, otlpTracesPrefix + suffix} {
			v, ok := r.get(name)
			if !ok {
				continue
			}
			if err := r.validateExporterVar(suffix, v); err != nil {
				r.fail(name, err)
			}
		}
	}

	// Client certificate and key are paired under the same prefix. The check
	// is skipped when either variable has its own error (AC-20).
	for _, prefix := range []string{otlpPrefix, otlpTracesPrefix} {
		certName, keyName := prefix+"CLIENT_CERTIFICATE", prefix+"CLIENT_KEY"
		if r.failed[certName] || r.failed[keyName] {
			continue
		}
		cp, cok := r.get(certName)
		kp, kok := r.get(keyName)
		switch {
		case cok && !kok:
			r.fail(keyName, errBadPair)
		case kok && !cok:
			r.fail(certName, errBadPair)
		case cok && kok && !r.keyPairOK(cp, kp):
			r.fail(keyName, errBadPair)
		}
	}

	if v, ok := r.get(varResourceAttrs); ok {
		if !validKeyValueList(v, false) {
			r.fail(varResourceAttrs, errBadKVList)
		}
	}
	// OTEL_SERVICE_NAME: any non-empty string is valid (empty is unset); nothing to check.

	// Sampler.
	if v, ok := r.get(varSampler); ok {
		switch k := SamplerKind(strings.ToLower(v)); k {
		case SamplerAlwaysOn, SamplerAlwaysOff, SamplerTraceIDRatio,
			SamplerParentBasedAlwaysOn, SamplerParentBasedAlwaysOff, SamplerParentBasedTraceIDRatio:
			cfg.Sampler = k
		default:
			r.fail(varSampler, errBadSampler)
		}
	}
	usesRatio := cfg.Sampler == SamplerUnset || cfg.Sampler == SamplerTraceIDRatio || cfg.Sampler == SamplerParentBasedTraceIDRatio
	if v, ok := r.get(varSamplerArg); ok && usesRatio {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
			r.fail(varSamplerArg, errBadSamplerArg)
		} else {
			cfg.SamplerArg = &f
			cfg.SampleRatio = f
		}
	}

	// Batch processor (FR-37).
	var err error
	if v, ok := r.get(varBSPQueue); ok {
		if n, err := parsePositiveInt(v); err != nil {
			r.fail(varBSPQueue, err)
		} else {
			cfg.Queue.MaxQueueSize = n
		}
	}
	batchSet := false
	if v, ok := r.get(varBSPBatch); ok {
		if n, err := parsePositiveInt(v); err != nil {
			r.fail(varBSPBatch, err)
		} else {
			cfg.Queue.MaxExportBatchSize = n
			batchSet = true
		}
	}
	if v, ok := r.get(varBSPDelay); ok {
		if cfg.Queue.ScheduleDelay, err = parsePositiveMillis(v); err != nil {
			r.fail(varBSPDelay, err)
		}
	}
	if v, ok := r.get(varBSPTimeout); ok {
		if cfg.Queue.ExportTimeout, err = parsePositiveMillis(v); err != nil {
			r.fail(varBSPTimeout, err)
		}
	}
	// The batch check involves both variables; it is skipped when either has
	// its own error (AC-20), never re-evaluated against a default.
	if !r.failed[varBSPQueue] && !r.failed[varBSPBatch] {
		switch {
		case batchSet && cfg.Queue.MaxExportBatchSize > cfg.Queue.MaxQueueSize:
			r.fail(varBSPBatch, errBatchTooLarge)
		case !batchSet && cfg.Queue.MaxExportBatchSize > cfg.Queue.MaxQueueSize:
			// Unset batch: effective value is min(default, queue).
			cfg.Queue.MaxExportBatchSize = cfg.Queue.MaxQueueSize
		}
	}
	return cfg
}

func (r *resolver) validateExporterVar(suffix, v string) error {
	switch suffix {
	case "ENDPOINT":
		if !validEndpoint(v) {
			return errBadEndpoint
		}
	case "HEADERS":
		if !validKeyValueList(v, true) {
			return errBadHeaders
		}
	case "TIMEOUT":
		// Non-negative integer milliseconds (D8).
		if n, err := strconv.ParseUint(v, 10, 63); err != nil || n > uint64(math.MaxInt64/int64(time.Millisecond)) {
			return errBadTimeout
		}
	case "COMPRESSION":
		if v != "gzip" && v != "none" {
			return errBadCompression
		}
	case "INSECURE":
		if !strings.EqualFold(v, "true") && !strings.EqualFold(v, "false") {
			return errBadBool
		}
	case "CERTIFICATE", "CLIENT_CERTIFICATE":
		if !r.certFileOK(v) {
			return errBadFile
		}
	case "CLIENT_KEY":
		if !r.keyFileOK(v) {
			return errBadFile
		}
	}
	return nil
}

func parsePositiveInt(v string) (int, error) {
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n <= 0 || n > maxQueueBound {
		return 0, errBadPositive
	}
	return int(n), nil
}

func parsePositiveMillis(v string) (time.Duration, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 || n > math.MaxInt64/int64(time.Millisecond) {
		return 0, errBadPositive
	}
	return time.Duration(n) * time.Millisecond, nil
}
