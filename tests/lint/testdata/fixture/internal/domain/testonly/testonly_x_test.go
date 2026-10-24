package testonly_test

import (
	"log/slog"
	"testing"

	"fixture/internal/infra/thing"
)

func TestXExempt(t *testing.T) { slog.Info(thing.Name) }
