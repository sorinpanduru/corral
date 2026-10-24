//go:build !unix

package markerscan

import "os"

// redirectFDs is a no-op where fd-level redirection is unavailable;
// CaptureStdio then only swaps os.Stdout and os.Stderr.
func redirectFDs(_, _ *os.File) (func(), error) { return func() {}, nil }
