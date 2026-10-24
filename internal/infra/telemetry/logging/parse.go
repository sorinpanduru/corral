package logging

import (
	"errors"
	"log/slog"
	"strings"
)

// Format is the log output format (FR-5).
type Format string

const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

// Sentinel errors carry constant messages and never echo the supplied value (FR-27).
var (
	ErrInvalidFormat = errors.New("logging: invalid log format")
	ErrInvalidLevel  = errors.New("logging: invalid log level")
)

// ParseFormat parses log.format. Errors never echo the supplied value (FR-27).
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(s) {
	case "json":
		return FormatJSON, nil
	case "text":
		return FormatText, nil
	}
	return "", ErrInvalidFormat
}

// ParseLevel parses log.level case-insensitively (FR-6). Offsets such as
// debug-4 are rejected. Errors never echo the supplied value (FR-27).
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, ErrInvalidLevel
}
