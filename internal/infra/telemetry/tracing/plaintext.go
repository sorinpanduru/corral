package tracing

import (
	"sort"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"google.golang.org/grpc/credentials/insecure"
)

// tlsMaterialSuffixes are the OTEL_EXPORTER_OTLP_[TRACES_]* variables that
// configure TLS material (SPEC-001 FR-20, Amendment 2).
var tlsMaterialSuffixes = []string{"CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY"}

// TLSIgnoredSettings implements the decision of SPEC-001 FR-41. It returns the
// names (never values) of the TLS variables that are set, sorted in ascending
// byte order, when the exporter is otlp and the effective traces endpoint (the
// _TRACES_ variable wins over the general one; scheme compared
// case-insensitively) uses http://. Otherwise, or when no TLS variable is set,
// it returns nil. Whitespace-only values count as unset, as in Resolve.
//
// Compliance (FR-24, FR-29): only variable names leave this function; paths and
// file contents are never read here. Resolve has already validated them (FR-27).
func TLSIgnoredSettings(cfg EnvConfig, env LookupEnv) []string {
	if cfg.Exporter != ExporterOTLP || env == nil {
		return nil
	}
	return tlsNamesForHTTP(env)
}

func tlsNamesForHTTP(env LookupEnv) []string {
	val := func(name string) string {
		v, _ := env(name)
		return strings.TrimSpace(v)
	}
	ep := val(otlpTracesPrefix + "ENDPOINT")
	if ep == "" {
		ep = val(otlpPrefix + "ENDPOINT")
	}
	if len(ep) < len("http://") || !strings.EqualFold(ep[:len("http://")], "http://") {
		return nil
	}
	var names []string
	for _, prefix := range []string{otlpPrefix, otlpTracesPrefix} {
		for _, s := range tlsMaterialSuffixes {
			if val(prefix+s) != "" {
				names = append(names, prefix+s)
			}
		}
	}
	sort.Strings(names)
	return names
}

// plaintextHTTPOptions override the TLS configuration the otlptracehttp
// exporter would otherwise build from the process environment: a nil TLS
// config replaces it, and the http:// scheme keeps the exporter insecure.
func plaintextHTTPOptions() []otlptracehttp.Option {
	return []otlptracehttp.Option{otlptracehttp.WithTLSClientConfig(nil), otlptracehttp.WithInsecure()}
}

// plaintextGRPCOptions replace the TLS credentials the otlptracegrpc exporter
// would otherwise build from the process environment.
func plaintextGRPCOptions() []otlptracegrpc.Option {
	return []otlptracegrpc.Option{
		otlptracegrpc.WithTLSCredentials(insecure.NewCredentials()),
		otlptracegrpc.WithInsecure(),
	}
}
