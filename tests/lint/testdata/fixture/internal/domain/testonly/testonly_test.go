package testonly

import (
	"log/slog"
	"testing"

	"example.test/fixture/internal/infra/thing"
)

func TestExempt(t *testing.T) { slog.Info(thing.Name) }
