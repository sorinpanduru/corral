package shadow

import (
	os "fixture.example/fx/fakeos"
)

// The identifier "os" here resolves to fakeos, not the standard library.
func Shadowed() {
	_ = os.Getenv("A")
	_, _ = os.Stat("f")
	_ = os.Args
}
