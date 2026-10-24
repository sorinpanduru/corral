// Package runtime is the composition root: it wires settings, logging, the
// third-party log bridges, metrics, tracing and the admin listener in the
// ADR-0006 startup order and runs the signal-driven graceful shutdown.
//
// Compliance (FR-24, GDPR data minimisation): every log message here is a
// constant; error details only ever come from config.LogError or errclass.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/app/configload"
	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/adminhttp"
	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/config/usage"
	"github.com/sorinpanduru/corral/internal/infra/sighup"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
)

// Constant lifecycle and failure messages (FR-7).
const (
	msgStarting        = "corral starting"
	msgStartupComplete = "corral startup complete"
	msgShuttingDown    = "corral shutting down"
	msgStartupFailed   = "corral startup failed"
	msgShutdownFailed  = "corral shutdown step failed"
	msgServeFailed     = "admin listener serve failed"
	msgPanic           = "recovered panic in runtime goroutine"
	// msgOTLPTLSIgnored is constant: no path, value or endpoint (FR-24, FR-29).
	msgOTLPTLSIgnored = "OTLP TLS settings ignored because the traces endpoint uses http://"
)

const tracingShutdownTimeout = 5 * time.Second

const adminShutdownTimeout = 5 * time.Second

// Exit codes (FR-24).
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 2
)

// Inputs are everything Run needs from the outside (FR-25): no process
// global is read.
type Inputs struct {
	Args     []string
	Env      []string // KEY=VALUE entries
	ReadFile source.ReadFile
	Stdout   io.Writer
	// HUP delivers SIGHUP; the entry point owns signal.Notify.
	HUP     <-chan os.Signal
	Version string
	// Registry selects the settings; nil means settings.Registry().
	Registry *registry.Registry
	// Now is the SIGHUP limiter clock; nil means time.Now.
	Now func() time.Time
}

// Hooks are in-process test hooks. All may be nil.
type Hooks struct {
	// BeforeLogger runs before the configuration is loaded, so before the
	// configured logger exists (AC-10). Help and version never reach it.
	BeforeLogger func(ctx context.Context)
	// BeforeCheckConfigExit runs in check-config mode after the FR-20 record
	// and before exit (AC-19).
	BeforeCheckConfigExit func(ctx context.Context)
	// BeforeLastStep runs after the admin listener serves and before it is
	// marked started (AC-31).
	BeforeLastStep func(ctx context.Context)
	// Started runs after the startup-complete record.
	Started func(Handles)
	// Stopping runs after the "shutting down" record and before the admin
	// server shuts down.
	Stopping func(ctx context.Context)
}

// Handles expose running components to Started.
type Handles struct {
	AdminAddr      net.Addr
	TracerProvider trace.TracerProvider
	Gatherer       prometheus.Gatherer
}

// Run starts corral and blocks until ctx is cancelled or startup fails. It
// returns the process exit code (ADR-0013): 0 after help, version, a
// successful check-config or a graceful shutdown (including a cancel during
// startup); 2 for a configuration error; 1 for any other startup or serve
// failure.
//
// Shutdown bound: after the "shutting down" record and the Stopping hook, the
// admin server gets at most adminShutdownTimeout and tracing at most
// tracingShutdownTimeout, so the whole shutdown takes about 10 s at worst.
func Run(ctx context.Context, in Inputs, hooks Hooks) int {
	stdout := in.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	// ADR-0013 step A: drain SIGHUP from the start, with no logger.
	watcher := sighup.Start(in.HUP, in.Now)
	defer watcher.Stop()

	settingsReg := in.Registry
	if settingsReg == nil {
		var err error
		if settingsReg, err = settings.Registry(); err != nil {
			logProgramming(stdout, err)
			return exitFailure
		}
	}

	// Step B: modes that start nothing.
	switch loader.SelectMode(in.Args) {
	case loader.ModeHelp:
		if err := usage.Write(stdout, settingsReg); err != nil {
			return exitFailure
		}
		return exitOK
	case loader.ModeVersion:
		version := in.Version
		if version == "" {
			version = "devel"
		}
		if _, err := fmt.Fprintf(stdout, "corral %s\n", version); err != nil {
			return exitFailure
		}
		return exitOK
	}

	// Step C: hold point before the configured logger exists.
	if hooks.BeforeLogger != nil {
		hooks.BeforeLogger(ctx)
	}

	// Step D: load and validate everything a run would.
	out := configload.Load(settingsReg, configload.Inputs{Args: in.Args, Env: in.Env, ReadFile: in.ReadFile})
	if out.Err != nil {
		logProgramming(stdout, out.Err)
		return exitFailure
	}
	if len(out.Errors) > 0 {
		// Failure path: the bootstrap JSON logger only (FR-29).
		boot := logging.BootstrapLogger(stdout)
		config.LogUnknownEnv(boot, out.Result.UnknownEnv)
		config.LogErrors(boot, out.Errors)
		return exitConfig
	}
	obs := out.Observability
	env := source.NewEnv(in.Env).Lookup

	// Success path, step 1: logging factory.
	var h slog.Handler = logging.NewHandler(stdout, obs.LogFormat, obs.LogLevel)
	if out.TracingEnabled {
		h = logging.WithTraceCorrelation(h)
	}
	factory := logging.NewFactory(h)

	// Step 2: third-party log bridges before any grpc/otel/tracing/admin code.
	logbridge.Install(factory, obs.LogLevel, time.Now)

	cfgLog := mustLogger(factory, logging.Config)
	life := mustLogger(factory, logging.Lifecycle)

	// Step 3: FR-8, FR-41 and FR-20 records (names only, never values).
	config.LogUnknownEnv(cfgLog, out.Result.UnknownEnv)
	if len(out.TLSIgnored) > 0 {
		cfgLog.Warn(msgOTLPTLSIgnored, slog.String("setting", strings.Join(out.TLSIgnored, ",")))
	}
	config.LogLoaded(cfgLog, out.Result.Supplied)

	// Check-config: validated, nothing starts. The watcher gets no logger, so
	// a SIGHUP never produces a record (FR-18, AC-19).
	if out.Mode == loader.ModeCheckConfig {
		if hooks.BeforeCheckConfigExit != nil {
			hooks.BeforeCheckConfigExit(ctx)
		}
		return exitOK
	}
	watcher.SetLogger(cfgLog)
	enabled, tcfg := out.TracingEnabled, out.Tracing

	// Step 4: lifecycle starting record.
	life.Info(msgStarting)

	// Step 5: metrics registry, factory and catalog registration.
	reg := metrics.NewRegistry()
	catalog, err := metrics.LoadCatalog()
	var mf *metrics.Factory
	if err == nil {
		mf, err = metrics.NewFactory(reg, catalog)
	}
	if err == nil {
		err = metrics.RegisterAll(mf)
	}
	if err != nil {
		logFailure(life, err)
		return exitFailure
	}

	// Step 6: admin listener (Listen + Serve), not yet ready.
	admin := adminhttp.New(adminhttp.Options{
		Addr:     obs.AdminListen,
		Gatherer: reg,
		ErrorLog: logbridge.NewHTTPErrorLog(factory, time.Now),
	})
	if err := admin.Listen(); err != nil {
		// The error is a config.Error: constant text, the net error only as
		// the hidden cause for errclass detail (never the address).
		config.LogError(cfgLog, err)
		return exitFailure
	}
	serveErr := make(chan error, 1)
	go func() {
		returned := false
		defer func() {
			if !returned { // panicked: recoverPanic (below) already logged it
				serveErr <- errServePanic
			}
		}()
		defer recoverPanic(life)
		if err := admin.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		returned = true
	}()

	// Step 7: BeforeLastStep hold point.
	if hooks.BeforeLastStep != nil {
		hooks.BeforeLastStep(ctx)
	}
	var tr *tracing.Tracing
	code := exitOK
	if ctx.Err() == nil {
		// Step 8: tracing.Start (last step).
		tr, err = tracing.Start(ctx, tracing.Options{
			Env:     env,
			Config:  tcfg,
			Enabled: enabled,
			Metrics: mf,
			Logger:  mustLogger(factory, logging.Tracing),
		})
		switch {
		case err != nil && ctx.Err() != nil:
			tr = nil // cancelled during startup: graceful path, no failure record
		case err != nil:
			logFailure(life, err)
			shutdownAdmin(life, admin)
			return exitFailure
		default:
			// Step 9: ready.
			admin.MarkStarted()
			life.Info(msgStartupComplete)
			if hooks.Started != nil {
				hooks.Started(Handles{AdminAddr: admin.Addr(), TracerProvider: tr.TracerProvider(), Gatherer: reg})
			}
			select {
			case <-ctx.Done():
			case err := <-serveErr:
				logServe(life, err)
				code = exitFailure
			}
		}
	}

	// Shutdown: MarkStopping immediately before the record (D6).
	admin.MarkStopping()
	life.Info(msgShuttingDown)
	if hooks.Stopping != nil {
		hooks.Stopping(ctx)
	}
	shutdownAdmin(life, admin)
	if tr != nil {
		shutdownTracing(life, tr)
	}
	return code
}

// logProgramming reports a programming error (never a configuration error)
// through the bootstrap logger: constant message, errclass attributes only.
func logProgramming(w io.Writer, err error) {
	logging.BootstrapLogger(w).LogAttrs(context.Background(), slog.LevelError, msgStartupFailed, errclass.LogAttrs(err)...)
}

// errServePanic is the constant failure delivered when the Serve goroutine panics.
var errServePanic = errors.New("admin serve goroutine panicked")

func shutdownAdmin(l *slog.Logger, admin *adminhttp.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
	defer cancel()
	if err := admin.Shutdown(ctx); err != nil {
		logShutdown(l, err)
	}
}

func shutdownTracing(l *slog.Logger, tr *tracing.Tracing) {
	ctx, cancel := context.WithTimeout(context.Background(), tracingShutdownTimeout)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		logShutdown(l, err)
	}
}

func logServe(l *slog.Logger, err error) {
	l.LogAttrs(context.Background(), slog.LevelError, msgServeFailed, errclass.LogAttrs(err)...)
}

func mustLogger(f *logging.Factory, c logging.Component) *slog.Logger {
	l, err := f.Logger(c)
	if err != nil { // unreachable: components are constants from the fixed set
		panic("runtime: component outside the fixed set")
	}
	return l
}

func logFailure(l *slog.Logger, err error) {
	l.LogAttrs(context.Background(), slog.LevelError, msgStartupFailed, errclass.LogAttrs(err)...)
}

func logShutdown(l *slog.Logger, err error) {
	l.LogAttrs(context.Background(), slog.LevelWarn, msgShutdownFailed, errclass.LogAttrs(err)...)
}

// recoverPanic must be deferred directly by a runtime goroutine.
func recoverPanic(l *slog.Logger) {
	if r := recover(); r != nil {
		err := errclass.FromPanic(r, debug.Stack())
		l.LogAttrs(context.Background(), slog.LevelError, msgPanic, errclass.LogAttrs(err)...)
	}
}
