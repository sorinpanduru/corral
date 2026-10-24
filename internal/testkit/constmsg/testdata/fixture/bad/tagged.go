//go:build functional

package bad

import "log/slog"

// Behind the functional build tag; the checker must still load this file.
func Tagged(l *slog.Logger, param string) {
	l.Info(param) // want
}
