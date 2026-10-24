package allow

import "log/slog"

// NonTest is a non-test file: the annotation is honoured only in _test.go
// files, so this correctly annotated non-constant call must still be reported.
func NonTest(p string) {
	//constmsg:allow simulated third-party caller
	slog.Info(p) // want
}
