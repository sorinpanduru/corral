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
	"strconv"
	"strings"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config"
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
	errBadHeaders     = errors.New("tracing: invalid key=value list")
	errBadTimeout     = errors.New("tracing: invalid timeout")
	errBadCompression = errors.New("tracing: unsupported compression")
	errBadBool        = errors.New("tracing: not a boolean")
	errBadFile        = errors.New("tracing: unreadable or non-PEM file")
	errBadPositive    = errors.New("tracing: not a positive integer")
	errBatchTooLarge  = errors.New("tracing: batch size exceeds queue size")
)

// Resolve decides whether tracing is on and, when it is, validates the OTEL_*
// variables (FR-19, FR-20, FR-21, FR-27, FR-37).
//
// Tracing is off when s.TracingEnabled is false or OTEL_SDK_DISABLED is "true"
// (case-insensitive). Any other OTEL_SDK_DISABLED value is false and never an
// error. When off, no other variable is read. When on, every honoured or
// validated variable is checked (both the general and _TRACES_ variants) and
// the first failure is returned as a config.Error naming the variable. An empty
// value counts as unset, as in the OpenTelemetry specification.
func Resolve(s config.Settings, env LookupEnv) (bool, EnvConfig, error) {
	if v, ok := env(varSDKDisabled); ok && strings.EqualFold(v, "true") {
		return false, EnvConfig{}, nil
	}
	if !s.TracingEnabled {
		return false, EnvConfig{}, nil
	}
	cfg := EnvConfig{
		Exporter:    ExporterOTLP,
		Protocol:    ProtocolHTTPProtobuf,
		SampleRatio: s.SampleRatio,
		Queue: QueueSettings{
			MaxQueueSize:       2048,
			MaxExportBatchSize: 512,
			ScheduleDelay:      5000 * time.Millisecond,
			ExportTimeout:      30000 * time.Millisecond,
		},
	}
	get := func(name string) (string, bool) {
		v, ok := env(name)
		return v, ok && v != ""
	}
	fail := func(name string, cause error) (bool, EnvConfig, error) {
		return false, EnvConfig{}, config.NewError(name, cause)
	}

	if v, ok := get(varExporter); ok {
		switch ExporterKind(v) {
		case ExporterOTLP, ExporterNone:
			cfg.Exporter = ExporterKind(v)
		default:
			return fail(varExporter, errBadExporter)
		}
	}

	// Protocol: the _TRACES_ variant wins, but both are validated.
	for _, name := range []string{otlpPrefix + "PROTOCOL", otlpTracesPrefix + "PROTOCOL"} {
		v, ok := get(name)
		if !ok {
			continue
		}
		switch Protocol(v) {
		case ProtocolHTTPProtobuf, ProtocolGRPC:
			cfg.Protocol = Protocol(v) // the later (_TRACES_) variant overrides
		default:
			return fail(name, errBadProtocol)
		}
	}

	// Remaining exporter variables: both variants validated, nothing kept.
	for _, suffix := range []string{"ENDPOINT", "HEADERS", "TIMEOUT", "COMPRESSION", "INSECURE", "CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY"} {
		for _, name := range []string{otlpPrefix + suffix, otlpTracesPrefix + suffix} {
			v, ok := get(name)
			if !ok {
				continue
			}
			if err := validateExporterVar(suffix, v); err != nil {
				return fail(name, err)
			}
		}
	}

	if v, ok := get(varResourceAttrs); ok {
		if !validKeyValueList(v) {
			return fail(varResourceAttrs, errBadHeaders)
		}
	}
	// OTEL_SERVICE_NAME: any non-empty string is valid (empty is unset); nothing to check.

	// Sampler.
	if v, ok := get(varSampler); ok {
		switch SamplerKind(v) {
		case SamplerAlwaysOn, SamplerAlwaysOff, SamplerTraceIDRatio,
			SamplerParentBasedAlwaysOn, SamplerParentBasedAlwaysOff, SamplerParentBasedTraceIDRatio:
			cfg.Sampler = SamplerKind(v)
		default:
			return fail(varSampler, errBadSampler)
		}
	}
	usesRatio := cfg.Sampler == SamplerUnset || cfg.Sampler == SamplerTraceIDRatio || cfg.Sampler == SamplerParentBasedTraceIDRatio
	if v, ok := get(varSamplerArg); ok && usesRatio {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
			return fail(varSamplerArg, errBadSamplerArg)
		}
		cfg.SamplerArg = &f
		cfg.SampleRatio = f
	}

	// Batch processor (FR-37).
	var err error
	if v, ok := get(varBSPQueue); ok {
		if cfg.Queue.MaxQueueSize, err = parsePositiveInt(v); err != nil {
			return fail(varBSPQueue, err)
		}
	}
	if v, ok := get(varBSPBatch); ok {
		if cfg.Queue.MaxExportBatchSize, err = parsePositiveInt(v); err != nil {
			return fail(varBSPBatch, err)
		}
	}
	if v, ok := get(varBSPDelay); ok {
		if cfg.Queue.ScheduleDelay, err = parsePositiveMillis(v); err != nil {
			return fail(varBSPDelay, err)
		}
	}
	if v, ok := get(varBSPTimeout); ok {
		if cfg.Queue.ExportTimeout, err = parsePositiveMillis(v); err != nil {
			return fail(varBSPTimeout, err)
		}
	}
	if cfg.Queue.MaxExportBatchSize > cfg.Queue.MaxQueueSize {
		return fail(varBSPBatch, errBatchTooLarge)
	}
	return true, cfg, nil
}

func validateExporterVar(suffix, v string) error {
	switch suffix {
	case "ENDPOINT":
		if !validEndpoint(v) {
			return errBadEndpoint
		}
	case "HEADERS":
		if !validKeyValueList(v) {
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
		if !certFileOK(v) {
			return errBadFile
		}
	case "CLIENT_KEY":
		if !keyFileOK(v) {
			return errBadFile
		}
	}
	return nil
}

func parsePositiveInt(v string) (int, error) {
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n <= 0 {
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
