// Package allow exercises the //constmsg:allow <reason> annotation. Lines
// ending in "// want" must be reported; every other call must not be.
package allow

import "log/slog"

func Calls(p string) {
	// Annotation on the preceding line, with a reason: covers the next line.
	//constmsg:allow simulated third-party caller
	slog.Info(p)

	slog.Info(p) //constmsg:allow simulated third-party caller

	// The preceding-line annotation covers only the one line after it, so the
	// second call is still reported.
	//constmsg:allow simulated third-party caller
	slog.Warn(p)
	slog.Warn(p) // want

	// A trailing annotation covers only its own line, not the next one.
	slog.Error(p) //constmsg:allow simulated third-party caller
	slog.Error(p) // want

	// An annotation without a reason does not suppress anything.
	//constmsg:allow
	slog.Debug(p) // want

	// An annotation for a different reason keyword is not an annotation.
	//constmsg:allowed simulated third-party caller
	slog.Debug(p) // want

	// An unannotated call far from any annotation is reported.
	slog.Info(p) // want
}
