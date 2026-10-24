package tracing_test

import (
	"os"
	"path/filepath"
	"testing"
)

func lookupOS(k string) (string, bool) { return os.LookupEnv(k) }

// envInvalidCase is one rejected value. value builds the supplied text from a
// random marker, so the no-echo check covers the whole rejection path.
type envInvalidCase struct {
	name    string
	setting string
	value   func(marker string) string
	extra   map[string]string // other variables needed to make the case meaningful
}

func fixed(s string) func(string) string { return func(string) string { return s } }
func withMarker(pre, post string) func(string) string {
	return func(m string) string { return pre + m + post }
}

// both lists a variable and its _TRACES_ variant.
func both(name, suffix string) []string {
	return []string{"OTEL_EXPORTER_OTLP_" + suffix, "OTEL_EXPORTER_OTLP_TRACES_" + suffix}
}

// envInvalidCases is the shared table of invalid values (AC-21 part 2, AC-16,
// D8). The first eight AC-21 values are marked.
func envInvalidCases(tb testing.TB) []envInvalidCase {
	tb.Helper()
	dir := tb.TempDir()
	nonPEM := filepath.Join(dir, "not-pem.txt")
	empty := filepath.Join(dir, "empty.txt")
	var cases []envInvalidCase
	add := func(name, setting string, v func(string) string, extra ...map[string]string) {
		c := envInvalidCase{name: name + " [" + setting + "]", setting: setting, value: v}
		if len(extra) > 0 {
			c.extra = extra[0]
		}
		cases = append(cases, c)
	}

	// AC-21 #1, B4: exporter.
	add("exporter console", "OTEL_TRACES_EXPORTER", fixed("console"))
	add("exporter otlp,none list", "OTEL_TRACES_EXPORTER", fixed("otlp,none"))
	add("exporter zipkin marker", "OTEL_TRACES_EXPORTER", withMarker("", ""))
	// AC-21 #2: sampler.
	add("sampler jaeger_remote", "OTEL_TRACES_SAMPLER", fixed("jaeger_remote"))
	add("sampler parentbased_jaeger_remote", "OTEL_TRACES_SAMPLER", fixed("parentbased_jaeger_remote"))
	add("sampler marker", "OTEL_TRACES_SAMPLER", withMarker("x", ""))
	// AC-21 #5, AC-16: sampler ARG, effective sampler uses a ratio.
	for _, s := range []map[string]string{nil, {"OTEL_TRACES_SAMPLER": "traceidratio"}, {"OTEL_TRACES_SAMPLER": "parentbased_traceidratio"}} {
		suffix := " default sampler"
		if s != nil {
			suffix = " " + s["OTEL_TRACES_SAMPLER"]
		}
		add("arg 1.5"+suffix, "OTEL_TRACES_SAMPLER_ARG", fixed("1.5"), s)
		add("arg -0.1"+suffix, "OTEL_TRACES_SAMPLER_ARG", fixed("-0.1"), s)
		add("arg NaN"+suffix, "OTEL_TRACES_SAMPLER_ARG", fixed("NaN"), s)
		add("arg Inf"+suffix, "OTEL_TRACES_SAMPLER_ARG", fixed("Inf"), s)
		add("arg non-number marker"+suffix, "OTEL_TRACES_SAMPLER_ARG", withMarker("0.", ""), s)
		add("arg out of range with marker"+suffix, "OTEL_TRACES_SAMPLER_ARG", withMarker("1", "e0x"), s)
	}

	// AC-21 #3: protocol (http/json is a config error).
	for _, v := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"} {
		add("protocol http/json", v, fixed("http/json"))
		add("protocol unknown marker", v, withMarker("proto-", ""))
		add("protocol empty-ish grpc+", v, fixed("grpc+tls"))
	}

	// Endpoint (D8): absolute http/https URL with a host.
	for _, v := range both("ENDPOINT", "ENDPOINT") {
		add("endpoint ftp scheme", v, fixed("ftp://collector.invalid:4318"))
		add("endpoint no scheme", v, fixed("collector.invalid:4318"))
		add("endpoint path only", v, fixed("/v1/traces"))
		add("endpoint no host", v, fixed("http://"))
		add("endpoint userinfo rejected scheme", v, withMarker("ftp://user:", "@collector.invalid:4318"))
		add("endpoint userinfo empty host", v, withMarker("http://user:", "@/v1/traces"))
		add("endpoint userinfo bad host char", v, withMarker("http://user:", "@bad host:4318/"))
		add("endpoint marker in path bad port", v, withMarker("http://collector.invalid:", ""))
		add("endpoint control char", v, withMarker("http://collector.invalid/", "\x7f\n"))
	}

	// AC-21 #4, D8: headers.
	for _, v := range both("HEADERS", "HEADERS") {
		add("headers no equals", v, withMarker("", ""))
		add("headers empty key marker value", v, withMarker("=", ""))
		add("headers bad percent value", v, withMarker("k=", "%zz"))
		add("headers truncated percent value", v, withMarker("k=", "%4"))
		add("headers bad percent in second", v, withMarker("a=b,Authorization=", "%g1"))
		add("headers entry without equals second", v, withMarker("a=b,", ""))
		add("headers bad percent key", v, withMarker("%zz", "=v"))
	}

	// Timeout (D8): non-negative integer ms.
	for _, v := range both("TIMEOUT", "TIMEOUT") {
		add("timeout negative", v, fixed("-1"))
		add("timeout fractional", v, fixed("1.5"))
		add("timeout unit suffix", v, withMarker("10", "s"))
		add("timeout overflow", v, fixed("99999999999999999999999"))
		add("timeout text", v, fixed("abc"))
	}

	// AC-21 #7: compression gzip|none.
	for _, v := range both("COMPRESSION", "COMPRESSION") {
		add("compression zstd", v, fixed("zstd"))
		add("compression deflate marker", v, withMarker("deflate-", ""))
	}
	// Insecure true|false.
	for _, v := range both("INSECURE", "INSECURE") {
		add("insecure yes", v, fixed("yes"))
		add("insecure 1", v, fixed("1"))
		add("insecure marker", v, withMarker("", ""))
	}

	// AC-21 #6: certificate / client certificate / client key (readable PEM).
	// The non-PEM file content carries the marker too: file contents are
	// never echoed. The files are created by the test (no committed fixtures).
	for _, base := range []string{"CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY"} {
		for _, v := range both(base, base) {
			v := v
			add("file non-PEM content", v, func(m string) string {
				_ = os.WriteFile(nonPEM, []byte("this is not PEM "+m+"\n"), 0o600)
				return nonPEM
			})
			add("file empty", v, func(string) string {
				_ = os.WriteFile(empty, nil, 0o600)
				return empty
			})
			add("file missing, marker in path", v, func(m string) string { return filepath.Join(dir, m, "absent.pem") })
			add("file is directory", v, func(string) string { return dir })
		}
	}

	// Resource attributes (D8): comma-separated key=value, valid percent-encoding.
	add("resource attrs no equals", "OTEL_RESOURCE_ATTRIBUTES", withMarker("", ""))
	add("resource attrs empty key", "OTEL_RESOURCE_ATTRIBUTES", withMarker("=", ""))
	add("resource attrs bad percent", "OTEL_RESOURCE_ATTRIBUTES", withMarker("a=", "%zz"))
	add("resource attrs second entry bad", "OTEL_RESOURCE_ATTRIBUTES", withMarker("a=b,", ""))

	// AC-21 #8, FR-37, D8: BSP variables, positive integers.
	for _, v := range []string{"OTEL_BSP_MAX_QUEUE_SIZE", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE", "OTEL_BSP_SCHEDULE_DELAY", "OTEL_BSP_EXPORT_TIMEOUT"} {
		add("bsp zero", v, fixed("0"))
		add("bsp negative", v, fixed("-5"))
		add("bsp fractional", v, fixed("1.5"))
		add("bsp overflow", v, fixed("99999999999999999999999"))
		add("bsp text marker", v, withMarker("", ""))
		add("bsp suffix marker", v, withMarker("10", "ms"))
	}
	return cases
}

// envValidCase is one accepted value.
type envValidCase struct {
	label, setting, value string
	extra                 map[string]string
}

func envValidCases(c envCerts) []envValidCase {
	var out []envValidCase
	add := func(setting, value string) {
		out = append(out, envValidCase{label: value, setting: setting, value: value})
	}
	for _, v := range both("ENDPOINT", "ENDPOINT") {
		for _, e := range []string{"http://localhost:4318", "https://collector.invalid:4317", "http://collector.invalid/v1/traces", "http://[::1]:4318", "http://127.0.0.1"} {
			add(v, e)
		}
	}
	for _, v := range both("HEADERS", "HEADERS") {
		for _, e := range []string{"k=v", "a=b,c=d", "a=b%20c", "x-api=syn%2Cthetic%3D1", "a=b"} {
			add(v, e)
		}
	}
	for _, v := range both("TIMEOUT", "TIMEOUT") {
		for _, e := range []string{"0", "1", "10000"} {
			add(v, e)
		}
	}
	for _, v := range both("COMPRESSION", "COMPRESSION") {
		add(v, "gzip")
		add(v, "none")
	}
	for _, v := range both("INSECURE", "INSECURE") {
		for _, e := range []string{"true", "false", "TRUE", "False"} {
			add(v, e)
		}
	}
	for _, v := range both("CERTIFICATE", "CERTIFICATE") {
		add(v, c.Cert)
	}
	for _, v := range both("CLIENT_CERTIFICATE", "CLIENT_CERTIFICATE") {
		add(v, c.Cert)
	}
	for _, v := range both("CLIENT_KEY", "CLIENT_KEY") {
		add(v, c.Key)
	}
	for _, e := range []string{"service.version=1.2.3", "a=b,c=d", "k=v%20w"} {
		add("OTEL_RESOURCE_ATTRIBUTES", e)
	}
	for _, v := range []string{"OTEL_BSP_MAX_QUEUE_SIZE", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE", "OTEL_BSP_SCHEDULE_DELAY", "OTEL_BSP_EXPORT_TIMEOUT"} {
		if v != "OTEL_BSP_MAX_QUEUE_SIZE" { // queue=1 would be below the default batch of 512
			add(v, "1")
		}
		if v == "OTEL_BSP_SCHEDULE_DELAY" || v == "OTEL_BSP_EXPORT_TIMEOUT" {
			add(v, "7500")
		}
	}
	add("OTEL_BSP_MAX_QUEUE_SIZE", "4096")
	add("OTEL_BSP_MAX_EXPORT_BATCH_SIZE", "256")
	add("OTEL_TRACES_SAMPLER_ARG", "0.0")
	add("OTEL_TRACES_SAMPLER_ARG", "1.0")
	add("OTEL_TRACES_SAMPLER_ARG", "0.5")
	return out
}
