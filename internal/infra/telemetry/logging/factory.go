package logging

import (
	"errors"
	"io"
	"log/slog"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
)

// ErrUnknownComponent is returned for a component outside the fixed set.
var ErrUnknownComponent = errors.New("logging: component not in the fixed set")

// Factory is the only way to obtain a logger (FR-7).
type Factory struct{ h slog.Handler }

// NewFactory returns a factory over h.
func NewFactory(h slog.Handler) *Factory { return &Factory{h: h} }

// Handler returns the underlying handler.
func (f *Factory) Handler() slog.Handler { return f.h }

// Logger returns a logger with component bound, or an error if c is outside
// the fixed set. The error does not echo c.
func (f *Factory) Logger(c Component) (*slog.Logger, error) {
	if !ValidComponent(c) {
		return nil, ErrUnknownComponent
	}
	return slog.New(f.h).With(attr.Component, string(c)), nil
}

// BootstrapLogger returns a JSON logger with component=config for errors
// raised before configuration (FR-27). Level is debug, so it filters nothing (SPEC-003 FR-29).
func BootstrapLogger(w io.Writer) *slog.Logger {
	return slog.New(NewHandler(w, FormatJSON, slog.LevelDebug)).With(attr.Component, string(Config))
}
