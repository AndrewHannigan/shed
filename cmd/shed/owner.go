package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
)

// newOwnerCmd groups the owner-management commands under an `owner` noun,
// mirroring the `repo` and `workspace` groups. A tracked owner is a whole
// GitHub user or org whose repos each sync discovers and auto-adds to the
// library.
//
// Unlike the `repo` group — where `repo add`/`repo rm` reuse the auto-detecting
// top-level commands verbatim — the owner forms commit to the owner reading:
// `owner add` forces owner tracking (so even an "owner/repo"-shaped argument is
// treated as an owner, not a single repo), and `owner rm` resolves names against
// owners only (top-level `shed rm` resolves against both). `owner ls` lists just
// the tracked owners, where plain `shed ls` also shows the repos and workspaces —
// the same split `repo ls` makes from the other side.
func newOwnerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "owner",
		Aliases: []string{"owners", "o"},
		Short:   "Manage tracked users/orgs (list, add, remove owners)",
	}
	cmd.AddCommand(
		newOwnerLsCmd(),
		newOwnerAddCmd(),
		newOwnerRmCmd(),
	)
	return cmd
}

func newOwnerLsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the tracked users/orgs in your library",
		Long: `ls lists the owners you track — whole GitHub users or orgs whose
repos sync discovers and adds to the library automatically — with how many
repos each currently manages.

Unlike 'shed ls', this shows only the owners: no repos, no workspaces. Run
'shed ls' for the full picture, or 'shed repo ls' for repos.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOwnerOnlyList(jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a human-readable table")
	return cmd
}

func newOwnerAddCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "add <owner>",
		Short: "Track a whole user/org (discover its repos via gh)",
		Long: `add tracks a GitHub user or org. <owner> may be a full URL
(https://github.com/octocat) or GitHub shorthand (a bare "octocat"), and is
checked against GitHub first so a typo can't become a dead entry that syncs
nothing.

On every sync, shed lists the owner's repos via gh and adds any new ones to
the library automatically. This is the same as 'shed add --owner <owner>';
under the 'owner' noun the owner reading is forced, so even an "owner/repo"
shaped argument is tracked as an owner rather than a single repo. See
'shed help owner'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// asOwner=true forces the owner reading regardless of the argument's
			// shape — that's the whole point of the owner-scoped form.
			return runRepoAdd(args[0], name, "", true, false)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "override the default name (derived from URL)")
	return cmd
}

func newOwnerRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <name>...",
		Short: "Remove tracked owners (and the repos they auto-added)",
		Long: `rm removes one or more tracked owners.

Removing an owner drops its entry and every repo it auto-added, along with
those repos' workspaces and stores. rm asks for confirmation first; answering
no keeps the repos — they stay on disk, just untied from the owner (Source
cleared) so a later sync no longer manages them.

Several names may be given at once ('shed owner rm a b c'); each is removed
independently, so a failure on one is reported but doesn't stop the rest.
Names resolve against owners only: a repo name here is "not in the config".

--force skips the confirmation prompt and discards uncommitted or unpushed
work without asking. When stdin is not a TTY, the owner is untied (its repos
kept) rather than deleted unattended.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOwnerRmMany(args, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"skip the confirmation prompt and delete even if a workspace has uncommitted or unpushed changes")
	return cmd
}

// runOwnerRmMany removes each named owner in turn, mirroring runRepoRmMany:
// each name is handled independently so a failure on one (a typo, or a
// workspace with unsaved work) is reported to stderr but does not stop the rest.
// A single name behaves exactly as a plain removal, its error propagating
// untouched to main for the "error:" line and the matching exit code. Duplicate
// names are collapsed so the same owner isn't removed (then reported gone) twice.
func runOwnerRmMany(names []string, force bool) error {
	names = dedupeStrings(names)
	if len(names) == 1 {
		return runOwnerRmByName(names[0], force)
	}
	var failed []error
	for _, name := range names {
		if err := runOwnerRmByName(name, force); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			failed = append(failed, err)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	// The per-name errors were already printed; return a concise summary so main
	// adds one closing line and exits non-zero, reusing the same exit-code rule
	// as the repo batch remover.
	return errs.New(rmBatchExitCode(failed), "%s could not be removed", pluralize(len(failed), "owner"))
}

// runOwnerRmByName resolves name against owners only (never repos) and removes
// the matching owner via the shared runOwnerRm. Scoping to owners is what
// distinguishes `owner rm` from top-level `shed rm`, which resolves against
// both and disambiguates: under the owner noun, a repo name is simply "not in
// the config".
func runOwnerRmByName(name string, force bool) error {
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	owner, err := c.ResolveOwner(name)
	if err != nil {
		return err // the owner "not found" message is already friendly
	}
	return runOwnerRm(owner, force)
}
