// Package hermetic has a cgo file (excluded when CGO_ENABLED=0) and a plain
// file with a constant message. It exists to prove the checker never runs a C
// toolchain or an external packages driver.
package hermetic

import "log/slog"

func Plain(l *slog.Logger) { l.Info("synthetic constant") }
