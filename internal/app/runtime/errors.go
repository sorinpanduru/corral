package runtime

import "errors"

// errBind is the constant cause attached to an admin bind failure; the
// unwrapped net error (which carries the address) is never logged.
var errBind = errors.New("admin listener bind failed")
