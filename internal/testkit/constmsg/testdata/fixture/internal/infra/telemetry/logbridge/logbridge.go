// Package logbridge sits at the one path allowed to call slog.NewLogLogger
// (it replaces the message with a constant); nothing here is reported.
package logbridge

import "log/slog"

func Make(h slog.Handler) { _ = slog.NewLogLogger(h, slog.LevelWarn) }
