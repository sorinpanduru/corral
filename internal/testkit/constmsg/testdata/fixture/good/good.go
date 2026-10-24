// Package good holds slog calls whose message is a compile-time constant, plus
// look-alike calls that are not slog. The checker must report nothing here.
package good

import (
	"context"
	"log/slog"
	sl "log/slog"
)

const prefix = "synthetic prefix"

type named string

const typed named = "synthetic typed"

const (
	a = "one"
	b = a + " two"
)

type fake struct{}

func (fake) Info(msg string, args ...any) {}
func (fake) Error(msg string)             {}

func Calls(ctx context.Context, l *slog.Logger, dynamic string) {
	// Constant literal.
	slog.Info("synthetic literal")
	l.Info("synthetic literal", "retry_count", 1)
	// Constant identifier, concatenation, conversion of a typed constant.
	slog.Debug(prefix)
	l.Warn(prefix + " suffix")
	l.Error(b)
	l.Info(string(typed))
	// Context variants, Log, LogAttrs, aliased import, derived loggers.
	slog.DebugContext(ctx, "synthetic")
	slog.InfoContext(ctx, "synthetic")
	slog.WarnContext(ctx, "synthetic")
	slog.ErrorContext(ctx, "synthetic")
	slog.Log(ctx, slog.LevelInfo, "synthetic")
	slog.LogAttrs(ctx, slog.LevelInfo, "synthetic", slog.Int("retry_count", 1))
	l.DebugContext(ctx, "synthetic")
	l.Log(ctx, slog.LevelWarn, "synthetic")
	l.LogAttrs(ctx, slog.LevelWarn, "synthetic")
	sl.Info("synthetic aliased")
	l.With("setting", "x").WithGroup("g").Info("synthetic derived")
	slog.Default().Info("synthetic default")
	// A dynamic value as an attribute value or message-less call is fine.
	l.Info("synthetic", "setting", dynamic)
	// Not slog: a type with a similarly named method is not checked.
	var f fake
	f.Info(dynamic)
	f.Error(dynamic + "x")
}
