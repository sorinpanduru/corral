package tracing

import (
	"strings"
	"testing"
)

func TestTLSIgnoredForHTTP(t *testing.T) {
	const p = "OTEL_EXPORTER_OTLP_"
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"http+ca", map[string]string{p + "ENDPOINT": "http://h:1", p + "CERTIFICATE": "x"}, true},
		{"http+traces-client", map[string]string{p + "ENDPOINT": "http://h:1", p + "TRACES_CLIENT_KEY": "x"}, true},
		{"traces-endpoint-wins-https", map[string]string{p + "ENDPOINT": "http://h:1", p + "TRACES_ENDPOINT": "https://h:1", p + "CERTIFICATE": "x"}, false},
		{"traces-endpoint-wins-http", map[string]string{p + "ENDPOINT": "https://h:1", p + "TRACES_ENDPOINT": "http://h:1", p + "CERTIFICATE": "x"}, true},
		{"https", map[string]string{p + "ENDPOINT": "https://h:1", p + "CERTIFICATE": "x"}, false},
		{"no-endpoint", map[string]string{p + "CERTIFICATE": "x"}, false},
		{"http-no-tls", map[string]string{p + "ENDPOINT": "http://h:1"}, false},
		{"empty-tls-is-unset", map[string]string{p + "ENDPOINT": "http://h:1", p + "CERTIFICATE": " "}, false},
	}
	for _, c := range cases {
		env := func(k string) (string, bool) { v, ok := c.env[k]; return v, ok }
		if got := len(TLSIgnoredSettings(EnvConfig{Exporter: ExporterOTLP}, env)) > 0; got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if TLSIgnoredSettings(EnvConfig{Exporter: ExporterNone}, func(string) (string, bool) { return "http://h", true }) != nil {
		t.Error("exporter none must not warn")
	}
}

func TestTLSIgnoredSettings_NamesSortedCaseInsensitiveScheme(t *testing.T) {
	const p = "OTEL_EXPORTER_OTLP_"
	m := map[string]string{
		p + "TRACES_ENDPOINT": "HTTP://h:1", p + "TRACES_CLIENT_KEY": "/x", p + "CLIENT_CERTIFICATE": "/x",
		p + "CERTIFICATE": "/x", p + "INSECURE": "false",
	}
	env := func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	got := strings.Join(TLSIgnoredSettings(EnvConfig{Exporter: ExporterOTLP}, env), ",")
	want := p + "CERTIFICATE," + p + "CLIENT_CERTIFICATE," + p + "TRACES_CLIENT_KEY"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	for _, ep := range []string{"localhost:4318", "ftp://h", "https://h"} {
		m[p+"TRACES_ENDPOINT"] = ep
		if TLSIgnoredSettings(EnvConfig{Exporter: ExporterOTLP}, env) != nil {
			t.Errorf("endpoint %q must not match", ep)
		}
	}
}
