package broken

import "os"

// This package does not type-check: the checker must report an error rather
// than a clean result for a tree it could not analyse.
func F() int { return os.Getenv("X") }
