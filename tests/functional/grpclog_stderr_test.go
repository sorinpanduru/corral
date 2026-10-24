//go:build functional

package functional

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// T24.3 (FR-24, FR-28, AC-3, AC-26, AC-29): google.golang.org/grpc/grpclog
// initialises before earlyslog and builds its default LoggerV2 over os.Stderr
// from GRPC_GO_LOG_SEVERITY_LEVEL / GRPC_GO_LOG_VERBOSITY_LEVEL. No grpc log
// output may reach stderr, before or after logbridge.Install. Every case also
// plants a synthetic marker in the endpoint (userinfo and path) and scans
// stdout and stderr.
func TestT24_GRPCLogNeverReachesStderr(t *testing.T) {
	severities := []string{"info", "warning", "error"}
	verbosities := []string{"", "99"}
	modes := []string{"unreachable", "reject", "off"}
	for _, mode := range modes {
		for _, sev := range severities {
			for _, vb := range verbosities {
				name := fmt.Sprintf("%s/sev=%s/verbosity=%s", mode, sev, vb)
				if vb == "" {
					name = fmt.Sprintf("%s/sev=%s/verbosity=unset", mode, sev)
				}
				t.Run(name, func(t *testing.T) { runGRPCLogCase(t, mode, sev, vb) })
			}
		}
	}
}

func runGRPCLogCase(t *testing.T, mode, sev, verbosity string) {
	marker := "T24GRPCMK" + strings.NewReplacer("/", "", "=", "").Replace(fmt.Sprintf("%s%s%s", mode, sev, verbosity)) + "x9q2"
	addr := freeAddr(t)
	env := []string{
		"GRPC_GO_LOG_SEVERITY_LEVEL=" + sev,
		"OTEL_EXPORTER_OTLP_PROTOCOL=grpc",
		"OTEL_EXPORTER_OTLP_TIMEOUT=300",
		"OTEL_BSP_SCHEDULE_DELAY=100",
		"OTEL_BSP_EXPORT_TIMEOUT=1000",
	}
	if verbosity != "" {
		env = append(env, "GRPC_GO_LOG_VERBOSITY_LEVEL="+verbosity)
	}
	args := []string{"-admin.listen=" + addr, "-log.level=debug"}
	var col *otlpcollector.Collector
	var hostport string
	switch mode {
	case "unreachable":
		hostport = otlpcollector.UnreachableEndpoint(t)
	case "reject":
		col = otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject),
			otlpcollector.WithGRPCRejectCode(codes.Unavailable))
		hostport = col.GRPCEndpoint()
	case "off":
		hostport = otlpcollector.UnreachableEndpoint(t)
	}
	env = append(env, "OTEL_EXPORTER_OTLP_ENDPOINT=http://user:"+marker+"@"+hostport+"/"+marker)
	if mode != "off" {
		args = append(args, "-tracing.enabled=true")
	}

	p := startProc(t, baseEnv(env...), args...)
	p.waitReady(addr)
	for i := 0; i < 5; i++ { // generate some spans / activity
		_, _ = httpDo("GET", addr, "/readyz")
		_, _ = httpDo("GET", addr, "/metrics")
	}
	time.Sleep(600 * time.Millisecond) // let the batch processor try to export
	if code := p.sigterm(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	if b := p.stderr.Bytes(); len(b) != 0 {
		t.Errorf("stderr has %d bytes, want empty (AC-3, FR-28)", len(b))
	}
	fixed := map[string]bool{
		logbridge.MsgGRPCDebug: true, logbridge.MsgGRPCWarn: true,
		logbridge.MsgGRPCError: true, logbridge.MsgGRPCFatal: true,
	}
	grpcRecs := 0
	for _, r := range p.records() { // fails on non-JSON stdout lines
		if str(r, "component") == "grpc" {
			grpcRecs++
			if !fixed[str(r, "msg")] {
				t.Errorf("component=grpc record has a message outside the fixed set (len %d)", len(str(r, "msg")))
			}
		}
	}
	// Non-vacuity: with tracing on, the grpc client bootstrap after
	// logbridge.Install logs through the bridge at debug (the bridge follows
	// log.level, not GRPC_GO_LOG_*), whatever the env severity or verbosity.
	if mode != "off" && grpcRecs == 0 {
		t.Error("no component=grpc record: the grpclog bridge was not exercised")
	}
	// Coverage: init-time grpclog behaviour and the grpc client bootstrap
	// after Install are exercised. The export itself is not, because the
	// current binary creates no spans (the collector may see no request).

	sc := markerscan.New()
	sc.AddMarker(t, marker)
	sc.Add(markerscan.SourceStdout, p.stdout.Bytes())
	sc.Add(markerscan.SourceStderr, p.stderr.Bytes())
	if col != nil {
		sc.AddCollector(col)
	}
	sc.AssertClean(t)
}
