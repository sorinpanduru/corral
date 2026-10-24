// Package clean uses the os package without any forbidden symbol. Its
// comments and strings mention os.Getenv and os.Args, which must not count.
package clean

import (
	"fmt"
	"os"
	"strings"
)

type env struct{}

// Getenv, Environ, Open and Stat are method or function names here, not the
// forbidden package-level symbols.
func (env) Getenv(string) string { return "" }
func (env) Environ() []string    { return nil }
func Open(string) error          { return nil }
func Stat(string) error          { return nil }

func Clean() {
	_ = "os.Getenv and os.Args in a string"
	_ = strings.ToUpper("syscall.Environ")
	fmt.Fprintln(os.Stderr, "x")
	_ = os.Stdout
	_ = os.Exit
	_ = env{}.Getenv("A")
	_ = Open("f")
	_ = Stat("f")
	// Methods of *os.File are not the package-level os.Stat or os.Open.
	if fi, err := os.Stdout.Stat(); err == nil {
		_ = fi
	}
	var f *os.File
	_ = f.Name
	_ = f.Close
}
