package registry_test

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/registry"
)

// Security: a FileValue never serialises its content (SPEC-003 FR-11, SPEC-002
// FR-24). JSON and text marshalling yield the path only, so no encoder that
// reaches a settings value can leak the file bytes (for example as base64).
func TestFileValueMarshalsPathOnly(t *testing.T) {
	const content = "synthetic-file-content-marker-Qx7Lm2Vt9Rw4"
	v := registry.FileValue{Path: "/run/secrets/example", Content: []byte(content)}
	for name, val := range map[string]any{"value": v, "pointer": &v, "in struct": struct{ F registry.FileValue }{v}, "in map": map[string]any{"f": v}} {
		b, err := json.Marshal(val)
		if err != nil {
			t.Fatalf("%s: json.Marshal: %v", name, err)
		}
		if !strings.Contains(string(b), "/run/secrets/example") {
			t.Errorf("%s: JSON %s lacks the path", name, b)
		}
		for _, enc := range []string{content, "Content", encodeB64(content)} {
			if strings.Contains(string(b), enc) {
				t.Errorf("%s: JSON %s contains %q", name, b, enc)
			}
		}
	}
	tm, ok := any(v).(encoding.TextMarshaler)
	if !ok {
		t.Fatal("FileValue does not implement encoding.TextMarshaler")
	}
	txt, err := tm.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(txt), "/run/secrets/example") || strings.Contains(string(txt), content) || strings.Contains(string(txt), encodeB64(content)) {
		t.Errorf("MarshalText = %q", txt)
	}
}

func encodeB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
