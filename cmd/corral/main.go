// Command corral is a MongoDB wire-protocol connection-pooling proxy.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/infra/osfile"
	// Must be linked: silences slog output of dependency initialisers (ADR-0008).
	_ "github.com/sorinpanduru/corral/internal/infra/telemetry/earlyslog"
)

// version is stamped by the linker: -ldflags "-X main.version=<v>" (Makefile).
var version = "devel"

func main() {
	// First statement (SPEC-003 FR-18): replace the default SIGHUP action
	// (terminate) before any argument is looked at.
	signal.Notify(hup, syscall.SIGHUP)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	reg, hooks := testBuild()
	code := runtime.Run(ctx, runtime.Inputs{
		Args:     os.Args[1:],
		Env:      os.Environ(),
		ReadFile: osfile.Read,
		Stdout:   os.Stdout,
		HUP:      hup,
		Version:  version,
		Registry: reg,
	}, hooks)
	stop()
	os.Exit(code)
}

// hup receives SIGHUP; buffered so a burst is not lost to the watcher.
var hup = make(chan os.Signal, 16)
