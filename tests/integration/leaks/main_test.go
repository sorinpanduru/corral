package leaks_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
)

// TestMain installs the production grpclog delegate once, before any test
// collector starts a gRPC server. grpclog.SetLoggerV2 is not safe against
// concurrent grpc activity, and logbridge.Install only calls it while the
// global does not already hold the delegate, so each runtime.Run afterwards
// merely retargets the delegate. After all tests it sweeps every buffer, late
// writes included, for every marker. Test infrastructure only.
func TestMain(m *testing.M) {
	retargetBridges()
	code := m.Run()
	if finalSweep() && code == 0 {
		code = 1
	}
	os.Exit(code)
}

// finalSweep scans ALL markers over ALL buffers of ALL runs (stdout buffers
// are read now, so writes after a test finished are included). It prints only
// the setup label, source and encoding name, never data, and reports whether
// anything was found.
func finalSweep() bool {
	registryMu.Lock()
	defer registryMu.Unlock()
	sc := markerscan.New()
	t := &sweepTB{}
	for m := range markers {
		sc.AddMarker(t, m)
	}
	for _, r := range runs {
		sc.Add(markerscan.SourceStdout, r.out.bytes())
		sc.Add(markerscan.SourceStdout, r.stdout)
		sc.Add(markerscan.SourceStderr, r.stderr)
		if r.coll != nil {
			sc.AddCollector(r.coll)
		}
	}
	found := false
	for _, f := range sc.Findings() {
		found = true
		fmt.Fprintf(os.Stderr, "FINAL SWEEP LEAK: marker of %q found in %s as %s\n", markers[f.Marker], f.Source, f.Encoding)
	}
	for _, src := range markerscan.AllSources() {
		if sc.Truncated(src) {
			found = true
			fmt.Fprintf(os.Stderr, "FINAL SWEEP: %s data truncated, scan incomplete\n", src)
		}
	}
	return found
}

// sweepTB satisfies testing.TB for AddMarker outside a test; it only needs to
// report failures.
type sweepTB struct{ testing.TB }

func (*sweepTB) Helper() {}
func (*sweepTB) Fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "final sweep: "+format+"\n", args...)
	os.Exit(1)
}
