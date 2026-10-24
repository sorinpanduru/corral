// Package bad deliberately violates FR-1: it imports log/slog and a
// non-stdlib, non-domain package.
package bad

import (
	"log/slog"

	"fixture/internal/infra/thing"
)

// Log is synthetic.
func Log() { slog.Info(thing.Name) }
