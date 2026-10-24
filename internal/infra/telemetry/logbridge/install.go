package logbridge

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/grpclog"
)

func installOTel(f *logging.Factory, now func() time.Time) {
	otel.SetErrorHandler(NewOTelErrorHandler(f, now))
	otel.SetLogger(logr.New(NewOTelLogSink(f, now)))
}

// probeLevel is a V level no grpc code uses; the delegate records it so that
// installGRPC can tell whether the grpclog global still holds the delegate.
const probeLevel = -1 << 30

// grpcDelegate is the process-wide grpclog.LoggerV2. grpclog.SetLoggerV2 is not
// safe while grpc goroutines run, so the global is set at most once per
// process (while it still holds the delegate) and later Install calls only
// swap the target atomically.
type grpcDelegate struct {
	target atomic.Pointer[grpclog.LoggerV2]
	probed atomic.Bool
}

var delegate = &grpcDelegate{}

func (d *grpcDelegate) t() grpclog.LoggerV2 { return *d.target.Load() }

func (d *grpcDelegate) Info(a ...any)               { d.t().Info(a...) }
func (d *grpcDelegate) Infoln(a ...any)             { d.t().Infoln(a...) }
func (d *grpcDelegate) Infof(f string, a ...any)    { d.t().Infof(f, a...) }
func (d *grpcDelegate) Warning(a ...any)            { d.t().Warning(a...) }
func (d *grpcDelegate) Warningln(a ...any)          { d.t().Warningln(a...) }
func (d *grpcDelegate) Warningf(f string, a ...any) { d.t().Warningf(f, a...) }
func (d *grpcDelegate) Error(a ...any)              { d.t().Error(a...) }
func (d *grpcDelegate) Errorln(a ...any)            { d.t().Errorln(a...) }
func (d *grpcDelegate) Errorf(f string, a ...any)   { d.t().Errorf(f, a...) }
func (d *grpcDelegate) Fatal(a ...any)              { d.t().Fatal(a...) }
func (d *grpcDelegate) Fatalln(a ...any)            { d.t().Fatalln(a...) }
func (d *grpcDelegate) Fatalf(f string, a ...any)   { d.t().Fatalf(f, a...) }
func (d *grpcDelegate) V(l int) bool {
	if l == probeLevel {
		d.probed.Store(true)
		return false
	}
	return d.t().V(l)
}

var grpcInstallMu sync.Mutex

func installGRPC(f *logging.Factory, level slog.Leveler, now func() time.Time) {
	grpcInstallMu.Lock()
	defer grpcInstallMu.Unlock()
	l := NewGRPCLogger(f, level, now)
	delegate.target.Store(&l)
	delegate.probed.Store(false)
	grpclog.V(probeLevel) // read-only: does the global still hold the delegate?
	if !delegate.probed.Load() {
		grpclog.SetLoggerV2(delegate)
	}
}
