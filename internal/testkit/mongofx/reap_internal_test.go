//go:build functional

package mongofx

import (
	"strings"
	"testing"
)

// The reaper removes a container only if it is labelled corralfx=1, its name
// starts with the prefix, and it is old enough: a labelled container without
// the name prefix is left alone. The test reaps with its own random prefix, so it
// cannot touch the fixtures of tests running in parallel, nor another run of
// itself.
func TestReaperNeedsLabelAndNamePrefix(t *testing.T) {
	requireDocker(t)
	// The random tag is part of the prefix the reaper is asked to use, so two
	// concurrent runs cannot remove each other's containers.
	prefix := "corralfx-reaptest-" + randHex(t, 4) + "-"
	mine := prefix + "a"                                              // labelled, prefixed
	other := "other-" + strings.TrimPrefix(prefix, "corralfx-") + "b" // labelled, NOT prefixed
	for _, name := range []string{mine, other} {
		out, err := docker("run", "-d", "--name", name, "--label", "corralfx=1", "--entrypoint", "sleep", Image("7.0"), "600")
		if err != nil {
			t.Fatalf("docker run %s: %s", name, out)
		}
		t.Cleanup(func() { remove(name) })
	}
	reapOlderThan(0, prefix)
	exists := func(name string) bool {
		out, _ := docker("ps", "-aq", "--filter", "name=^"+name+"$")
		return strings.TrimSpace(out) != ""
	}
	if exists(mine) {
		t.Error("a labelled, prefixed, old-enough container was not reaped")
	}
	if !exists(other) {
		t.Error("a labelled container without the name prefix was removed")
	}
}

func TestFixtureNamesUseThePrefix(t *testing.T) {
	if namePrefix != "corralfx-" {
		t.Fatalf("namePrefix = %q", namePrefix)
	}
}
