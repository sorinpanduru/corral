// Package bad holds slog calls whose message is not a compile-time constant.
// Each violating call line ends with a "want" marker comment, which the test
// compares with the reported lines.
package bad

import (
	"context"
	"fmt"
	"log/slog"
	sl "log/slog"
	"time"
)

var fv = slog.Info // want value

func two() (context.Context, string) { return context.Background(), "synthetic" }

var global = "synthetic global"

type holder struct{ msg string }

func Calls(ctx context.Context, l *slog.Logger, param string, h holder, err error) {
	slog.Info(param)                                  // want
	slog.Debug(fmt.Sprintf("synthetic %d", 1))        // want
	slog.Warn("synthetic " + param)                   // want
	slog.Error(global)                                // want
	slog.InfoContext(ctx, h.msg)                      // want
	slog.DebugContext(ctx, param)                     // want
	slog.WarnContext(ctx, param)                      // want
	slog.ErrorContext(ctx, err.Error())               // want
	slog.Log(ctx, slog.LevelInfo, param)              // want
	slog.LogAttrs(ctx, slog.LevelInfo, param)         // want
	l.Info(param)                                     // want
	l.Debug(param, "retry_count", 1)                  // want
	l.Warn(fmt.Sprint(err))                           // want
	l.Error(err.Error())                              // want
	l.InfoContext(ctx, param)                         // want
	l.DebugContext(ctx, param)                        // want
	l.WarnContext(ctx, param)                         // want
	l.ErrorContext(ctx, param)                        // want
	l.Log(ctx, slog.LevelWarn, param)                 // want
	l.LogAttrs(ctx, slog.LevelWarn, param)            // want
	sl.Info(param)                                    // want
	l.With("setting", "x").WithGroup("g").Info(param) // want
	slog.Default().Info(param)                        // want
	l.Info(string([]byte(param)))                     // want

	// Used as a value: the call target cannot be tracked, so the use is reported.
	f := l.Info // want value
	f(param)
	fv(param)

	// Method expression: reported on the message argument.
	(*slog.Logger).Log(l, ctx, slog.LevelInfo, param) // want
	// Record construction with a non-constant message.
	_ = slog.NewRecord(time.Time{}, slog.LevelInfo, param, 0) // want
	// Multi-value spread: the message cannot be proven constant (fail closed).
	slog.InfoContext(two()) // want

	// Constant calls in the same function are not reported.
	l.Info("synthetic constant")
	slog.Info("synthetic constant")
}
