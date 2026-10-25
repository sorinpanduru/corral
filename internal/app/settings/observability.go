package settings

import (
	"log/slog"

	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// Logical names of the SPEC-001 settings.
const (
	LogFormat          = "log.format"
	LogLevel           = "log.level"
	AdminListen        = "admin.listen"
	TracingEnabled     = "tracing.enabled"
	TracingSampleRatio = "tracing.sample_ratio"
	observabilitySpec  = "SPEC-001"
	defaultAdminListen = "127.0.0.1:9187"
	defaultSampleRatio = "1.0"
)

// ObservabilityGroup is the SPEC-001 group (exported for in-process tests that
// build a registry without the proxy group, ADR-0021).
func ObservabilityGroup() registry.Group {
	enum := func(vs ...string) types.Spec { return types.Spec{Kind: types.KindEnum, EnumValues: vs} }
	def := func(name string, t types.Spec, d, desc string) registry.Decl {
		return registry.Decl{Name: name, Type: t, Presence: registry.PresenceDefault, Default: d, Description: desc}
	}
	ratio := def(TracingSampleRatio, types.Spec{Kind: types.KindFloat}, defaultSampleRatio, "Trace sampling ratio.")
	ratio.Rule = func(v any) error {
		if f, ok := v.(float64); !ok || f < 0 || f > 1 {
			return errRatioRange
		}
		return nil
	}
	return registry.Group{
		Spec:       observabilitySpec,
		Namespaces: []string{"log", "admin", "tracing"},
		Decls: []registry.Decl{
			def(LogFormat, enum("json", "text"), "json", "Log output format."),
			def(LogLevel, enum("debug", "info", "warn", "error"), "info", "Minimum log level."),
			def(AdminListen, types.Spec{Kind: types.KindHostPort}, defaultAdminListen, "Admin HTTP listener address."),
			def(TracingEnabled, types.Spec{Kind: types.KindBool}, "false", "Enable OTLP tracing."),
			ratio,
		},
	}
}

// Observability is the typed view of the SPEC-001 settings (FR-16).
type Observability struct {
	LogFormat      logging.Format
	LogLevel       slog.Level
	AdminListen    string
	TracingEnabled bool
	SampleRatio    float64
}

// DefaultObservability returns the declared defaults.
func DefaultObservability() Observability {
	return Observability{
		LogFormat:      logging.FormatJSON,
		LogLevel:       slog.LevelInfo,
		AdminListen:    defaultAdminListen,
		TracingEnabled: false,
		SampleRatio:    1.0,
	}
}

// ObservabilityFrom maps a load result to the typed view. Callers must check
// res.Errors first: for a failed setting this returns the default, which is
// not a usable configuration. A value that is missing or of an unexpected type
// keeps its default.
func ObservabilityFrom(res loader.Result) Observability {
	o := DefaultObservability()
	if v, ok := res.Get(LogFormat); ok {
		if s, _ := v.(string); s == "text" {
			o.LogFormat = logging.FormatText
		} else if s == "json" {
			o.LogFormat = logging.FormatJSON
		}
	}
	if v, ok := res.Get(LogLevel); ok {
		switch s, _ := v.(string); s {
		case "debug":
			o.LogLevel = slog.LevelDebug
		case "info":
			o.LogLevel = slog.LevelInfo
		case "warn":
			o.LogLevel = slog.LevelWarn
		case "error":
			o.LogLevel = slog.LevelError
		}
	}
	if v, ok := res.Get(AdminListen); ok {
		if s, ok := v.(string); ok {
			o.AdminListen = s
		}
	}
	if v, ok := res.Get(TracingEnabled); ok {
		if b, ok := v.(bool); ok {
			o.TracingEnabled = b
		}
	}
	if v, ok := res.Get(TracingSampleRatio); ok {
		if f, ok := v.(float64); ok {
			o.SampleRatio = f
		}
	}
	return o
}
