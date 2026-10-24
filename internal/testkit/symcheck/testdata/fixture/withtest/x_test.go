package withtest

import (
	"os"
	"testing"
)

// Test files are out of scope: tests legitimately use t.Setenv and os.*.
func TestX(t *testing.T) { _ = os.Getenv("A") }
