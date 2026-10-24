// Package cfgbad deliberately violates SPEC-003 FR-16: a domain package that
// imports the settings registry and the loader.
package cfgbad

import (
	"cfgfix/internal/infra/config/loader"
	"cfgfix/internal/infra/config/registry"
)

// Names is synthetic.
var Names = registry.Name + loader.Name
