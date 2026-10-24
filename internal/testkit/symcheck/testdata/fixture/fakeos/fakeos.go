// Package fakeos mimics the names of the os package. It is a different
// package and must never be reported.
package fakeos

var Args []string

func Getenv(string) string                   { return "" }
func LookupEnv(string) (string, bool)        { return "", false }
func Environ() []string                      { return nil }
func ReadFile(string) ([]byte, error)        { return nil, nil }
func Open(string) (int, error)               { return 0, nil }
func OpenFile(string, int, int) (int, error) { return 0, nil }
func Stat(string) (int, error)               { return 0, nil }
func Lstat(string) (int, error)              { return 0, nil }
