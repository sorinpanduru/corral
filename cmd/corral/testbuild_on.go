//go:build corraltest

package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// testBuild is the corraltest build (ADR-0014): CORRALTEST_REGISTRY adds a
// test group (namespace t, never part of settings.Groups()), and
// CORRALTEST_HOLD_DIR with CORRALTEST_HOLD_AT selects a hold hook. These
// variables are CORRALTEST_*, not CORRAL_*, so they are never CORRAL
// variables under FR-8. Synthetic test code only.
func testBuild() (*registry.Registry, runtime.Hooks) {
	var hooks runtime.Hooks
	if dir := os.Getenv("CORRALTEST_HOLD_DIR"); dir != "" {
		switch os.Getenv("CORRALTEST_HOLD_AT") {
		case "logger":
			hooks.BeforeLogger = func(ctx context.Context) { hold(ctx, dir) }
		case "checkconfig":
			hooks.BeforeCheckConfigExit = func(ctx context.Context) { hold(ctx, dir) }
		}
	}
	var extra *registry.Group
	switch os.Getenv("CORRALTEST_REGISTRY") {
	case "secret":
		extra = &registry.Group{Spec: "TEST", Namespaces: []string{"t"}, Decls: []registry.Decl{{
			Name: "t.secret", Type: types.Spec{Kind: types.KindText}, Presence: registry.PresenceOptional,
			Secret: true, Description: "Synthetic test secret.",
		}}}
	case "required":
		extra = &registry.Group{Spec: "TEST", Namespaces: []string{"t"}, Decls: []registry.Decl{{
			Name: "t.required", Type: types.Spec{Kind: types.KindString}, Presence: registry.PresenceRequired,
			Description: "Synthetic required test setting.",
		}}}
	}
	if extra == nil {
		return nil, hooks
	}
	reg, err := registry.New(append(settings.Groups(), *extra)...)
	if err != nil {
		os.Exit(1)
	}
	return reg, hooks
}

// hold creates <dir>/held, then waits (at most 30 s) for <dir>/release.
func hold(ctx context.Context, dir string) {
	_ = os.WriteFile(filepath.Join(dir, "held"), nil, 0o600)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
