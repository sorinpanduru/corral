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
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/adminhttp"
	"github.com/sorinpanduru/corral/internal/infra/config"
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
)

const tracingShutdownTimeout = 5 * time.Second

const adminShutdownTimeout = 5 * time.Second

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
)

// Hooks are in-process test hooks. Both may be nil.
type Hooks struct {
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
// returns the process exit code: 0 after a graceful shutdown (including a
// cancel during startup), non-zero on any startup or serve failure.
//
// Shutdown bound: after the "shutting down" record and the Stopping hook, the
// admin server gets at most adminShutdownTimeout and tracing at most
// tracingShutdownTimeout, so the whole shutdown takes about 10 s at worst.
func Run(ctx context.Context, args []string, env tracing.LookupEnv, stdout io.Writer, hooks Hooks) int {
	// ADR-0006 step 0: settings (stdlib-only logging until the factory exists).
	raw, err := config.FromArgs(args)
	var settings config.Settings
	if err == nil {
		settings, err = config.Parse(raw)
	}
	if err != nil {
		config.LogError(logging.BootstrapLogger(stdout), err)
		return exitFailure
	}

	// Resolve is pure environment parsing; it runs before the handler is built
	// so trace correlation is wrapped only when tracing is really on. Its error
	// is logged after Install (step 3).
	enabled, tcfg, resolveErr := tracing.Resolve(settings, env)

	// Step 1: logging factory.
	var h slog.Handler = logging.NewHandler(stdout, settings.LogFormat, settings.LogLevel)
	if resolveErr == nil && enabled {
		h = logging.WithTraceCorrelation(h)
	}
	factory := logging.NewFactory(h)

	// Step 2: third-party log bridges before any grpc/otel/tracing/admin code.
	logbridge.Install(factory, settings.LogLevel, time.Now)

	cfgLog := mustLogger(factory, logging.Config)
	life := mustLogger(factory, logging.Lifecycle)

	// Step 3: tracing environment validation result.
	if resolveErr != nil {
		config.LogError(cfgLog, resolveErr)
		return exitFailure
	}

	// Step 4: lifecycle starting record.
	life.Info(msgStarting)

	// Steps 5-6: metrics registry and factory.
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

	// Step 7: admin listener, bound and serving (not yet ready).
	admin := adminhttp.New(adminhttp.Options{
		Addr:     settings.AdminListen,
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
		defer recoverPanic(life)
		if err := admin.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	// Step 8: hold point for tests, then tracing as the last step.
	if hooks.BeforeLastStep != nil {
		hooks.BeforeLastStep(ctx)
	}
	var tr *tracing.Tracing
	code := exitOK
	if ctx.Err() == nil {
		tr, err = tracing.Start(ctx, tracing.Options{
			Settings: settings,
			Env:      env,
			Config:   tcfg,
			Enabled:  enabled,
			Metrics:  mf,
			Logger:   mustLogger(factory, logging.Tracing),
		})
		if err != nil {
			logFailure(life, err)
			shutdownAdmin(life, admin)
			return exitFailure
		}

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
