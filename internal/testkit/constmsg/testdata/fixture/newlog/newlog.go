// Package newlog is outside the logbridge path: slog.NewLogLogger is reported.
package newlog

import "log/slog"

func Make(h slog.Handler) {
	_ = slog.NewLogLogger(h, slog.LevelWarn) // want
	//constmsg:allow simulated third-party caller
	_ = slog.NewLogLogger(h, slog.LevelInfo) // want
}
