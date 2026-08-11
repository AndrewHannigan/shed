package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// version is set at link time by goreleaser via -ldflags "-X main.version=…".
// Default "dev" is what you see when running `go run` / a bare `go build`.
var version = "dev"

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var coded *errs.Coded
		if errors.As(err, &coded) {
			os.Exit(coded.Code)
		}
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "shed",
		Short:         "git repo management for terminal coding agents",
		Long:          rootLong,
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		// Gate every real command on `shed init` having run. Like the PostRun
		// below, cobra runs the closest Persistent hook up the tree and no
		// subcommand defines one, so this fires for every leaf. It runs before
		// the command's own RunE, so an uninitialized install fails fast with a
		// clear "run shed init" message instead of behaving like an empty
		// catalog. Commands that must work pre-init are exempted (see initExempt).
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			if initExempt(c) || paths.Initialized() {
				return nil
			}
			return errs.New(errs.Config, "shed is not initialized; run 'shed init' first")
		},
		// Record successful "working" commands to the history log. PostRun (not
		// the …E variant) so logging can never alter exit status; cobra runs the
		// closest Persistent hook up the tree, and no subcommand defines one, so
		// this fires for every leaf — on success only (a failed RunE skips it).
		PersistentPostRun: func(c *cobra.Command, _ []string) { recordCommand(c) },
	}
	cmd.SetHelpCommand(newHelpCmd())
	// Make a bare `shed`, `shed -h`, and `shed --help` all print the same
	// curated overview that `shed help` prints. Without this, the flag form
	// falls back to Cobra's auto-generated usage — a different topline and
	// layout from `shed help`, which is confusing. Subcommand help
	// (`shed add --help`) still uses Cobra's default rendering.
	defaultHelp := cmd.HelpFunc()
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		if c == cmd {
			fmt.Fprint(c.OutOrStdout(), helpTopics["overview"])
			return
		}
		defaultHelp(c, args)
	})
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(
		newInitCmd(),
		newAddCmd(),
		newRmCmd(),
		newLsCmd(),
		newRepoCmd(),
		newOwnerCmd(),
		newSyncCmd(),
		newStatusCmd(),
		newWorkspaceCmd(),
		newPathCmd(),
		newPruneCmd(),
		newHistoryCmd(),
		newSessionContextCmd(),
		newOnToolCallCmd(),
		newBgSyncCmd(),
		newWelcomeTourCmd(),
		newResumeCmd(),
	)
	// Grouping commands (workspace, repo, owner) print their help and exit 0 for
	// ANY input by default, which silently swallows a mistyped subcommand like
	// `shed ws add`. Make them reject an unknown subcommand with a clear error,
	// the same way the root already does.
	rejectUnknownSubcommands(cmd)
	return cmd
}

// rejectUnknownSubcommands makes every grouping command under root fail on an
// unrecognized subcommand instead of silently printing its help and exiting 0.
//
// A grouping command (workspace, repo, owner) only namespaces subcommands and
// has no action of its own, so cobra leaves it non-runnable — and a non-runnable
// command prints its help and exits 0 for ANY arguments. That means a mistyped
// subcommand like `shed ws add` shows the menu and "succeeds", hiding the typo.
// The root command doesn't have this problem (cobra's legacyArgs turns an
// unknown top-level word into an "unknown command" error); attaching a RunE
// gives every grouping command the same treatment — a bare invocation still
// prints help, but an unknown subcommand is a clear error with a non-zero exit.
//
// Making these commands runnable would otherwise subject them to the init gate;
// initExempt keeps grouping commands exempt so the bare-help and unknown-command
// paths behave identically before and after `shed init`.
func rejectUnknownSubcommands(parent *cobra.Command) {
	for _, child := range parent.Commands() {
		if !child.HasSubCommands() {
			continue
		}
		child.RunE = func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return c.Help()
			}
			return errs.New(errs.Config, "unknown command %q for %q%s",
				args[0], c.CommandPath(), subcommandSuggestions(c, args[0]))
		}
		rejectUnknownSubcommands(child) // cover any nested groups too
	}
}

// subcommandSuggestions mirrors cobra's unexported (*Command).findSuggestions so
// a grouping command's "unknown command" error reads exactly like the one cobra
// emits at the root: a "Did you mean this?" block of near-matches, or "" when
// there are none (or suggestions are disabled).
func subcommandSuggestions(cmd *cobra.Command, arg string) string {
	if cmd.DisableSuggestions {
		return ""
	}
	// cobra defaults this distance lazily inside findSuggestions; do the same so
	// close typos (not just prefixes) are offered.
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	suggestions := cmd.SuggestionsFor(arg)
	if len(suggestions) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nDid you mean this?\n")
	for _, s := range suggestions {
		fmt.Fprintf(&sb, "\t%v\n", s)
	}
	return sb.String()
}

// initExempt reports whether c may run before `shed init` has set up the config
// and data dirs. `init` itself bootstraps them; `help` documents the tool and
// must work on a fresh install; and the hidden `__*` hook commands are invoked
// by agents (sometimes before any manual init) and must never fail the gate and
// disrupt a session — they already treat a missing store as empty.
//
// Grouping commands (workspace, repo, owner) are exempt too: they never touch
// the store themselves — they only print help or reject an unknown subcommand
// (see rejectUnknownSubcommands) — so they should behave the same before and
// after init, like help does. Their leaf subcommands (`workspace new`, …) have
// no subcommands of their own and so remain gated.
//
// Cobra routes `--help` and `--version` to help without reaching a
// PersistentPreRun, so those need no entry here.
func initExempt(c *cobra.Command) bool {
	return c.Name() == "init" || c.Name() == "help" ||
		strings.HasPrefix(c.Name(), "__") || c.HasSubCommands()
}

const rootLong = `shed maintains read-only local checkouts of GitHub repos —
backed by one shared mirror per upstream — and carves writable
workspaces off them with fast, offline-capable local clones.

Designed for terminal coding agents (Claude Code, Cursor, opencode)
to search across many repos and edit a few.`
