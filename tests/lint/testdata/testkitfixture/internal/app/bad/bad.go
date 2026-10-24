// Package bad deliberately violates T21: production code importing testkit
// and the stand-in for golang.org/x/tools.
package bad

import (
	"tkfixture/external/tools"
	"tkfixture/internal/testkit/kit"
)

// Name is synthetic.
const Name = kit.Name + tools.Name
