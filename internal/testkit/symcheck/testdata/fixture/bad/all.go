package bad

import (
	"os"
	"syscall"
)

// Each line marked "want:" must be reported exactly once with that symbol.
func All() {
	_ = os.Args                   // want: os.Args
	_ = os.Getenv("A")            // want: os.Getenv
	_, _ = os.LookupEnv("A")      // want: os.LookupEnv
	_ = os.Environ()              // want: os.Environ
	_ = syscall.Getenv            // want: syscall.Getenv
	_ = syscall.Environ()         // want: syscall.Environ
	_, _ = os.ReadFile("f")       // want: os.ReadFile
	_, _ = os.Open("f")           // want: os.Open
	_, _ = os.OpenFile("f", 0, 0) // want: os.OpenFile
	_, _ = os.Stat("f")           // want: os.Stat
	_, _ = os.Lstat("f")          // want: os.Lstat
}
