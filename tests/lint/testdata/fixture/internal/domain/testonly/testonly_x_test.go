package testonly_test

import (
	"log/slog"
	"testing"

	"example.test/fixture/internal/infra/thing"
)

func TestXExempt(t *testing.T) { slog.Info(thing.Name) }
