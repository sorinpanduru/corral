// Package runtime is the composition root: it wires settings, logging, the
// third-party log bridges, metrics, tracing and the admin listener in the
// ADR-0006 startup order and runs the signal-driven graceful shutdown.
//
// Compliance (FR-24, GDPR data minimisation): every log message here is a
// constant; error details only ever come from config.LogError or errclass.
package runtime

import (
	"context"
	"io"
	"log/slog"
	"net"
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
	msgPanic           = "recovered panic in runtime goroutine"
)

const tracingShutdownTimeout = 5 * time.Second

// shutdownDrain is a short pause between the "shutting down" record and the
// admin server shutdown. net/http's Shutdown drops a request whose head
// completes after Shutdown began, so without it a probe racing the record
// would see a closed connection instead of 503 shutting_down.
const shutdownDrain = 250 * time.Millisecond

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
}

// Handles expose running components to Started.
type Handles struct {
	AdminAddr      net.Addr
	TracerProvider trace.TracerProvider
	Gatherer       prometheus.Gatherer
}

// Run starts corral and blocks until ctx is cancelled or startup fails. It
// returns the process exit code: 0 after a graceful shutdown, non-zero on any
// startup failure.
func Run(ctx context.Context, args []string, env tracing.LookupEnv, stdout io.Writer, hooks Hooks) int {
	// Step 0: settings (stdlib-only logging until the factory exists).
	raw, err := config.FromArgs(args)
	var settings config.Settings
	if err == nil {
		settings, err = config.Parse(raw)
	}
	if err != nil {
		config.LogError(logging.BootstrapLogger(stdout), err)
		return exitFailure
	}

	// Step 1: logging factory.
	var h slog.Handler = logging.NewHandler(stdout, settings.LogFormat, settings.LogLevel)
	if settings.TracingEnabled {
		h = logging.WithTraceCorrelation(h)
	}
	factory := logging.NewFactory(h)

	// Step 2: third-party log bridges before any grpc/otel/tracing/admin code.
	logbridge.Install(factory, settings.LogLevel, time.Now)

	cfgLog := mustLogger(factory, logging.Config)
	life := mustLogger(factory, logging.Lifecycle)

	// Step 3: tracing environment.
	enabled, tcfg, err := tracing.Resolve(settings, env)
	if err != nil {
		config.LogError(cfgLog, err)
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

	// Step 7: tracing.
	tr, err := tracing.Start(ctx, tracing.Options{
		Settings: settings,
		Env:      env,
		Config:   tcfg,
		Enabled:  enabled,
		Metrics:  mf,
		Logger:   mustLogger(factory, logging.Tracing),
	})
	if err != nil {
		logFailure(life, err)
		return exitFailure
	}

	// Step 8: admin listener.
	admin := adminhttp.New(adminhttp.Options{
		Addr:     settings.AdminListen,
		Gatherer: reg,
		ErrorLog: logbridge.NewHTTPErrorLog(factory, time.Now),
	})
	if err := admin.Listen(); err != nil {
		// Only the constant config.Error text is logged, never the cause.
		config.LogError(cfgLog, config.NewError(config.SettingAdminListen, errBind))
		shutdownTracing(life, tr)
		return exitFailure
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		defer recoverPanic(life)
		_ = admin.Serve()
	}()

	// Step 9: last step, then ready.
	if hooks.BeforeLastStep != nil {
		hooks.BeforeLastStep(ctx)
	}
	admin.MarkStarted()
	life.Info(msgStartupComplete)
	if hooks.Started != nil {
		hooks.Started(Handles{AdminAddr: admin.Addr(), TracerProvider: tr.TracerProvider(), Gatherer: reg})
	}

	// Shutdown.
	<-ctx.Done()
	admin.MarkStopping()
	life.Info(msgShuttingDown)
	time.Sleep(shutdownDrain)
	sctx, cancel := context.WithTimeout(context.Background(), tracingShutdownTimeout)
	if err := admin.Shutdown(sctx); err != nil {
		logShutdown(life, err)
	}
	cancel()
	<-serveDone
	shutdownTracing(life, tr)
	return exitOK
}

func shutdownTracing(l *slog.Logger, tr *tracing.Tracing) {
	ctx, cancel := context.WithTimeout(context.Background(), tracingShutdownTimeout)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		logShutdown(l, err)
	}
}

func mustLogger(f *logging.Factory, c logging.Component) *slog.Logger {
	l, err := f.Logger(c)
	if err != nil { // unreachable: components are constants from the fixed set
		return slog.New(f.Handler())
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
