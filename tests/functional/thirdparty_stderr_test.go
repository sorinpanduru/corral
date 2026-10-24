//go:build functional

package functional

import (
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// sdkEnvNotValidatedByCorral are OTel SDK environment variables that corral
// does not validate itself; the SDK parses them on its own.
var sdkEnvNotValidatedByCorral = []string{
	"OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT",
	"OTEL_SPAN_EVENT_COUNT_LIMIT",
	"OTEL_SPAN_LINK_COUNT_LIMIT",
	"OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT",
	"OTEL_LINK_ATTRIBUTE_COUNT_LIMIT",
	"OTEL_ATTRIBUTE_COUNT_LIMIT",
	"OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT",
	"OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT",
}

// T22 (FR-24, FR-27, FR-28, Amendment 1 thirdparty): with tracing enabled and
// SDK limit variables set to non-numeric garbage, corral still starts, stderr
// stays empty, the supplied raw value appears in no output, and every stdout
// line is JSON; any record that originates from third-party code carries
// component=thirdparty and a message that does not echo the value.
func TestT22_SDKGarbageEnvNeverReachesStderr(t *testing.T) {
	const marker = "T22-SYNTH-MARKER-zq81xv"
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto, func(t *testing.T) {
			addr := freeAddr(t)
			ep := otlpcollector.UnreachableEndpoint(t)
			env := []string{
				"OTEL_EXPORTER_OTLP_ENDPOINT=http://" + ep,
				"OTEL_EXPORTER_OTLP_PROTOCOL=" + proto,
			}
			for _, k := range sdkEnvNotValidatedByCorral {
				env = append(env, k+"="+marker)
			}
			p := startProc(t, baseEnv(env...), "-tracing.enabled=true", "-admin.listen="+addr)
			p.waitReady(addr) // FR-24: startup succeeds
			if code := p.sigterm(); code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
			if b := p.stderr.Bytes(); len(b) != 0 {
				t.Errorf("stderr has %d bytes, want empty (FR-28)", len(b))
			}
			for name, b := range map[string][]byte{"stdout": p.stdout.Bytes(), "stderr": p.stderr.Bytes()} {
				if strings.Contains(string(b), marker) {
					t.Errorf("%s contains the supplied raw value (FR-24)", name)
				}
				for _, k := range sdkEnvNotValidatedByCorral {
					if strings.Contains(string(b), k) {
						t.Errorf("%s echoes variable name %s (FR-24)", name, k)
					}
				}
			}
			for _, r := range p.records() { // fails on non-JSON stdout lines
				if str(r, "component") == "thirdparty" && str(r, "msg") == "" {
					t.Errorf("thirdparty record has empty msg")
				}
			}
		})
	}
}

// Each variable on its own, so a failure names which one leaks.
func TestT22_EachSDKVarAlone(t *testing.T) {
	const marker = "T22-SYNTH-MARKER-k3p0wd"
	vars := append([]string{
		"OTEL_BSP_SCHEDULE_DELAY", "OTEL_BSP_EXPORT_TIMEOUT",
		"OTEL_BSP_MAX_QUEUE_SIZE", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE",
	}, sdkEnvNotValidatedByCorral...)
	for _, k := range vars {
		t.Run(k, func(t *testing.T) {
			addr := freeAddr(t)
			ep := otlpcollector.UnreachableEndpoint(t)
			p := startProc(t, baseEnv("OTEL_EXPORTER_OTLP_ENDPOINT=http://"+ep, k+"="+marker),
				"-tracing.enabled=true", "-admin.listen="+addr)
			// Either corral refuses to start (it validates this one itself, FR-24)
			// or it becomes ready; only the no-leak property is pinned here.
			ready := false
			for deadline := time.Now().Add(waitFor); time.Now().Before(deadline) && !ready; {
				select {
				case <-p.done:
					deadline = time.Time{}
				default:
					if r, err := httpDo("GET", addr, "/readyz"); err == nil && r.status == 200 {
						ready = true
					} else {
						time.Sleep(25 * time.Millisecond)
					}
				}
			}
			if ready {
				p.sigterm()
			}
			<-p.done
			if b := p.stderr.Bytes(); len(b) != 0 {
				t.Errorf("stderr has %d bytes, want empty", len(b))
			}
			if strings.Contains(string(p.stdout.Bytes()), marker) {
				t.Errorf("stdout contains the supplied raw value")
			}
		})
	}
}
