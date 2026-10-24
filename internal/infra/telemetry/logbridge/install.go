package logbridge

import (
	"log/slog"
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

func installGRPC(f *logging.Factory, level slog.Leveler, now func() time.Time) {
	grpclog.SetLoggerV2(NewGRPCLogger(f, level, now))
}
