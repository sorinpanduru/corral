package loader_test

import (
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/loader"
)

// SPEC-003 FR-27 / AC-12 (unit part): control-flag mode selection and the
// combinations table, through SelectMode plus ParseArgs.

func TestModesAreDistinct(t *testing.T) {
	ms := []loader.Mode{loader.ModeRun, loader.ModeHelp, loader.ModeVersion, loader.ModeCheckConfig}
	for i, a := range ms {
		for j, b := range ms {
			if i != j && a == b {
				t.Fatalf("modes %d and %d are equal", i, j)
			}
		}
	}
}

// FR-27 spellings.
func TestSelectMode_Spellings(t *testing.T) {
	for arg, want := range map[string]loader.Mode{
		"--help": loader.ModeHelp, "-help": loader.ModeHelp, "-h": loader.ModeHelp,
		"--version": loader.ModeVersion, "-version": loader.ModeVersion,
		"--check-config": loader.ModeCheckConfig, "-check-config": loader.ModeCheckConfig,
	} {
		if got := loader.SelectMode([]string{arg}); got != want {
			t.Errorf("SelectMode(%q) = %v, want %v", arg, got, want)
		}
	}
}

// Everything else is a normal run: variants, values, case, whitespace.
func TestSelectMode_NotSpellings(t *testing.T) {
	for _, arg := range []string{
		"", "t.x=1", "--h", "-v", "--v", "--HELP", "-Help", "--Version", "--CHECK-CONFIG", "--check_config",
		"help", "version", "check-config", "-help ", " -h", "-h ", "--help=x", "--help=", "-help=1", "-h=1",
		"--version=", "--version=1", "-version=x", "--check-config=true", "-check-config=", "---help", "--he lp", "-?", "--usage",
		"--log.level=help", "--t.x=-h",
	} {
		if got := loader.SelectMode([]string{arg}); got != loader.ModeRun {
			t.Errorf("SelectMode(%q) = %v, want ModeRun", arg, got)
		}
	}
	if got := loader.SelectMode(nil); got != loader.ModeRun {
		t.Errorf("SelectMode(nil) = %v, want ModeRun", got)
	}
	if got := loader.SelectMode([]string{}); got != loader.ModeRun {
		t.Errorf("SelectMode(empty) = %v, want ModeRun", got)
	}
}

// FR-27 combinations table, and precedence help > version > check-config.
func TestSelectMode_CombinationsTable(t *testing.T) {
	cases := []struct {
		args []string
		want loader.Mode
	}{
		{[]string{"--help", "--version"}, loader.ModeHelp},
		{[]string{"--version", "--check-config"}, loader.ModeVersion},
		{[]string{"--check-config", "--help"}, loader.ModeHelp},
		{[]string{"--help", "--bogus=1"}, loader.ModeHelp},
		{[]string{"--version", "--log.level=bad"}, loader.ModeVersion},
		{[]string{"--check-config", "--check-config"}, loader.ModeCheckConfig},
		{[]string{"--check-config=true"}, loader.ModeRun},
		{[]string{"--check-config", "--help=x"}, loader.ModeCheckConfig},
		// precedence and position independence
		{[]string{"--version", "-h"}, loader.ModeHelp},
		{[]string{"-check-config", "-version", "-help"}, loader.ModeHelp},
		{[]string{"-check-config", "-version"}, loader.ModeVersion},
		{[]string{"-version", "-check-config"}, loader.ModeVersion},
		{[]string{"--check-config", "-check-config"}, loader.ModeCheckConfig},
		{[]string{"--help", "--help", "-h", "-help"}, loader.ModeHelp},
		{[]string{"--version", "-version"}, loader.ModeVersion},
		// everything else is ignored, valid or not
		{[]string{"positional", "--help"}, loader.ModeHelp},
		{[]string{"--", "-h"}, loader.ModeHelp},
		{[]string{"-", "--version"}, loader.ModeVersion},
		{[]string{"--t.x", "--version", "--t.x"}, loader.ModeVersion},
		{[]string{"--t.secret=v", "--help"}, loader.ModeHelp},
		{[]string{"--help=x", "--version"}, loader.ModeVersion},
		{[]string{"--version=", "-h"}, loader.ModeHelp},
		{[]string{"--help=x", "--check-config"}, loader.ModeCheckConfig},
		{[]string{"--version=1", "--help=2", "--check-config=3"}, loader.ModeRun},
		{[]string{"--bogus", "--check-config", "pos"}, loader.ModeCheckConfig},
		{[]string{"--log.level=debug", "--tracing.enabled=true"}, loader.ModeRun},
		{[]string{"--t.x=--help"}, loader.ModeRun},
	}
	for _, c := range cases {
		if got := loader.SelectMode(c.args); got != c.want {
			t.Errorf("SelectMode(%d args %s) = %v, want %v", len(c.args), strings.Join(c.args, " "), got, c.want)
		}
	}
}

// The combination rows that run the parser: FR-27 "Records" column.
func TestFR27_TableThroughParseArgs(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		mode  loader.Mode
		probs []prob
	}{
		{"--check-config=true", []string{"--check-config=true"}, loader.ModeRun,
			[]prob{{0, unknown, loader.ErrControlFlagValue}}},
		{"--check-config --help=x", []string{"--check-config", "--help=x"}, loader.ModeCheckConfig,
			[]prob{{1, unknown, loader.ErrControlFlagValue}}},
		{"--check-config --check-config", []string{"--check-config", "--check-config"}, loader.ModeCheckConfig, nil},
		{"--check-config -check-config", []string{"--check-config", "-check-config"}, loader.ModeCheckConfig, nil},
		{"--version= in a normal run", []string{"--version="}, loader.ModeRun,
			[]prob{{0, unknown, loader.ErrControlFlagValue}}},
		{"check-config with other errors", []string{"--check-config", "--help=x", "--bogus", "pos", "--tracing.enabled"},
			loader.ModeCheckConfig,
			[]prob{{1, unknown, loader.ErrControlFlagValue}, {2, unknown, loader.ErrUnknownFlag}, {3, argument, loader.ErrPositional},
				{4, "tracing.enabled", loader.ErrMissingValue}}},
		{"--h is unknown", []string{"--h"}, loader.ModeRun, []prob{{0, unknown, loader.ErrUnknownFlag}}},
		{"-h=1 is unknown", []string{"-h=1"}, loader.ModeRun, []prob{{0, unknown, loader.ErrControlFlagValue}}},
		{"repeated control flags are fine", []string{"--check-config", "-check-config", "--check-config"}, loader.ModeCheckConfig, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := loader.SelectMode(c.args); got != c.mode {
				t.Fatalf("mode = %v, want %v", got, c.mode)
			}
			_, pr, _ := parse(t, c.args...)
			requireProblems(t, pr, c.probs)
		})
	}
}
