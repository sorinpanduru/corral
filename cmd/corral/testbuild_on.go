//go:build corraltest

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	dir := os.Getenv("CORRALTEST_HOLD_DIR")
	points := map[string]bool{}
	if dir != "" {
		for _, p := range strings.Split(os.Getenv("CORRALTEST_HOLD_AT"), ",") {
			points[strings.TrimSpace(p)] = true
		}
		if points["logger"] {
			hooks.BeforeLogger = func(ctx context.Context) { hold(ctx, dir, "held", "release") }
		}
		if points["checkconfig"] {
			hooks.BeforeCheckConfigExit = func(ctx context.Context) { hold(ctx, dir, "held", "release") }
		}
		hooks.Hold = func(ctx context.Context, point string) {
			if points[point] {
				hold(ctx, dir, "held-"+point, "release-"+point)
			}
		}
	}
	hooks.PoolRoundRobin = os.Getenv("CORRALTEST_POOL_ORDER") == "roundrobin"
	hooks.PanicCommand = os.Getenv("CORRALTEST_PANIC_COMMAND")
	if addr := os.Getenv("CORRALTEST_PROBE_LISTEN"); addr != "" {
		hooks.Probe = startProbe(addr, dir)
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

// hold creates <dir>/<held>, then waits (at most 30 s) for <dir>/<release>.
func hold(ctx context.Context, dir, held, release string) {
	_ = os.WriteFile(filepath.Join(dir, held), nil, 0o600)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if _, err := os.Stat(filepath.Join(dir, release)); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
