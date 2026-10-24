// Package mid imports testing from a non-test file, so any binary that
// imports mid transitively links testing.
package mid

import "testing"

// Verbose is synthetic.
func Verbose() bool { return testing.Verbose() }
