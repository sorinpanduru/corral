// Package thing stands in for a non-stdlib, non-domain package. It imports
// another non-stdlib package so tests can pin direct-imports-only behaviour.
package thing

import "fixture/internal/infra/deep"

// Name is synthetic.
var Name = "thing" + deep.Name
