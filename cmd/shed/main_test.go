package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// The init gate exempts exactly the commands that must run before `shed init`:
// init itself (it does the bootstrapping), help (docs on a fresh install), the
// hidden __* hook commands (invoked by agents, sometimes before any manual
// init), and the grouping commands (workspace/repo/owner), which only print help
// or reject an unknown subcommand and so never touch the store. Every other
// (leaf) command is gated. Commands are built directly rather than via root.Find
// because cobra adds the help command lazily during Execute.
func TestInitExempt(t *testing.T) {
	exempt := []*cobra.Command{
		newInitCmd(),
		newHelpCmd(),
		newBgSyncCmd(),
		newOnToolCallCmd(),
		newSessionContextCmd(),
		newWelcomeTourCmd(),
		newWorkspaceCmd(),
		newRepoCmd(),
		newOwnerCmd(),
	}
	for _, c := range exempt {
		if !initExempt(c) {
			t.Errorf("%q should be exempt from the init gate", c.Name())
		}
	}

	gated := []*cobra.Command{
		newAddCmd(),
		newRmCmd(),
		newLsCmd(),
		newSyncCmd(),
		newStatusCmd(),
		newPruneCmd(),
		newHistoryCmd(),
		newResumeCmd(),
	}
	for _, c := range gated {
		if initExempt(c) {
			t.Errorf("%q must be gated on init, not exempt", c.Name())
		}
	}
}

// A real command run before `shed init` fails fast with a message pointing at
// `shed init`, instead of silently behaving like an empty catalog.
func TestInitGateBlocksUninitialized(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if paths.Initialized() {
		t.Fatal("a fresh temp env should not look initialized")
	}

	root := newRootCmd()
	root.SetArgs([]string{"ls"})
	err := root.Execute()
	if err == nil {
		t.Fatal("`shed ls` before init should be blocked by the gate")
	}
	if !strings.Contains(err.Error(), "shed init") {
		t.Errorf("gate error should point at `shed init`, got: %v", err)
	}
}

// Once the config file and data dir init creates exist, the gate lets a command
// through (the help command, exempt by name, is unaffected either way).
func TestInitGateAllowsAfterInit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// runInit is the real bootstrap; suppress its progress output.
	var initErr error
	captureStdout(t, func() { initErr = runInit() })
	if initErr != nil {
		t.Fatalf("runInit: %v", initErr)
	}
	if !paths.Initialized() {
		t.Fatal("after runInit the env should look initialized")
	}

	root := newRootCmd()
	root.SetArgs([]string{"ls"})
	if err := root.Execute(); err != nil {
		t.Fatalf("`shed ls` after init should pass the gate, got: %v", err)
	}
}

// A mistyped subcommand of a grouping command (workspace/repo/owner) must fail
// with a clear "unknown command" error and a non-zero exit, not silently print
// the group's help and exit 0 — the cobra default for a non-runnable parent.
// The exit code travels on the *errs.Coded error so main turns it into status.
func TestUnknownSubcommandErrors(t *testing.T) {
	for _, args := range [][]string{
		{"workspace", "add", "blocks", "projects"}, // the reported case
		{"ws", "add"}, // via the alias
		{"repo", "bogus"},
		{"owner", "bogus"},
	} {
		root := newRootCmd()
		root.SetArgs(args)
		err := root.Execute()
		if err == nil {
			t.Errorf("`shed %s`: want an error on the unknown subcommand, got nil", strings.Join(args, " "))
			continue
		}
		if !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("`shed %s`: want an \"unknown command\" error, got: %v", strings.Join(args, " "), err)
		}
		var coded *errs.Coded
		if !errors.As(err, &coded) || coded.Code != errs.Config {
			t.Errorf("`shed %s`: want exit code %d, got: %v", strings.Join(args, " "), errs.Config, err)
		}
	}
}

// A close typo offers the cobra-style "Did you mean this?" suggestions, the same
// way an unknown top-level command does at the root.
func TestUnknownSubcommandSuggests(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"workspace", "ne"}) // a near-miss for "new"
	err := root.Execute()
	if err == nil {
		t.Fatal("`shed workspace ne` should error")
	}
	if !strings.Contains(err.Error(), "Did you mean this?") || !strings.Contains(err.Error(), "new") {
		t.Errorf("expected a suggestion of \"new\", got: %v", err)
	}
}

// A bare grouping command still prints its help and exits 0 — only an unknown
// subcommand is an error. This must hold even before `shed init` (groups are
// exempt from the init gate), matching cobra's old non-runnable behavior.
func TestBareGroupPrintsHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if paths.Initialized() {
		t.Fatal("a fresh temp env should not look initialized")
	}

	var execErr error
	out := captureStdout(t, func() {
		root := newRootCmd()
		root.SetArgs([]string{"workspace"})
		execErr = root.Execute()
	})
	if execErr != nil {
		t.Fatalf("bare `shed workspace` should not error, got: %v", execErr)
	}
	if !strings.Contains(out, "Available Commands") {
		t.Errorf("bare `shed workspace` should print its help, got:\n%s", out)
	}
}
