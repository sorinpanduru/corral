package leaks_test

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// TestMain installs the production grpclog delegate once, before any test
// collector starts a gRPC server. grpclog.SetLoggerV2 is not safe against
// concurrent grpc activity, and logbridge.Install only calls it while the
// global does not already hold the delegate, so each runtime.Run afterwards
// merely retargets the delegate. Test infrastructure only.
func TestMain(m *testing.M) {
	h := logging.NewHandler(io.Discard, "json", slog.LevelError)
	logbridge.Install(logging.NewFactory(h), slog.LevelError, time.Now)
	os.Exit(m.Run())
}
