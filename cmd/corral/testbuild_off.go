//go:build !corraltest

package main

import (
	"github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
)

// testBuild is the production build: the default registry and no hooks
// (ADR-0014).
func testBuild() (*registry.Registry, runtime.Hooks) { return nil, runtime.Hooks{} }
