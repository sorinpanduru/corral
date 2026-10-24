package tracing_test

import (
	"io"
	"os"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/grpclog"
)

// grpclog.SetLoggerV2 is not thread-safe and earlier tests' collector gRPC
// goroutines may still read the logger, so it is set exactly once per test
// binary, to a delegate whose target is swapped atomically per test.
type grpcDelegate struct {
	target atomic.Pointer[grpclog.LoggerV2]
}

var grpcLog = func() *grpcDelegate {
	d := &grpcDelegate{}
	d.set(grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard))
	return d
}()

func (d *grpcDelegate) set(l grpclog.LoggerV2) { d.target.Store(&l) }
func (d *grpcDelegate) get() grpclog.LoggerV2  { return *d.target.Load() }

func (d *grpcDelegate) Info(a ...any)               { d.get().Info(a...) }
func (d *grpcDelegate) Infoln(a ...any)             { d.get().Infoln(a...) }
func (d *grpcDelegate) Infof(f string, a ...any)    { d.get().Infof(f, a...) }
func (d *grpcDelegate) Warning(a ...any)            { d.get().Warning(a...) }
func (d *grpcDelegate) Warningln(a ...any)          { d.get().Warningln(a...) }
func (d *grpcDelegate) Warningf(f string, a ...any) { d.get().Warningf(f, a...) }
func (d *grpcDelegate) Error(a ...any)              { d.get().Error(a...) }
func (d *grpcDelegate) Errorln(a ...any)            { d.get().Errorln(a...) }
func (d *grpcDelegate) Errorf(f string, a ...any)   { d.get().Errorf(f, a...) }
func (d *grpcDelegate) Fatal(a ...any)              { d.get().Fatal(a...) }
func (d *grpcDelegate) Fatalln(a ...any)            { d.get().Fatalln(a...) }
func (d *grpcDelegate) Fatalf(f string, a ...any)   { d.get().Fatalf(f, a...) }
func (d *grpcDelegate) V(l int) bool                { return d.get().V(l) }

func TestMain(m *testing.M) {
	grpclog.SetLoggerV2(grpcLog)
	os.Exit(m.Run())
}
