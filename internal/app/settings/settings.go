// Package settings holds the declarations of every spec's settings (SPEC-003
// ADR-0011): one registry.Group per spec, plus the typed view of each group
// that the application layer hands to the domain and infrastructure.
//
// Compliance (SPEC-003 FR-11, GDPR data minimisation): declarations hold names,
// types, defaults and descriptions only; no value from a running process.
package settings

import (
	"errors"

	"github.com/sorinpanduru/corral/internal/infra/config/registry"
)

var errRatioRange = errors.New("settings: ratio out of range")

// Groups lists every spec's group. A new spec adds exactly one group here.
func Groups() []registry.Group {
	return []registry.Group{ObservabilityGroup(), ProxyGroup()}
}

// Registry builds the registry of every group.
func Registry() (*registry.Registry, error) {
	return registry.New(Groups()...)
}
