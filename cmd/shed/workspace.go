package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/catalog"
	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

func newWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workspace",
		Aliases: []string{"ws"},
		Short:   "Manage writable workspaces derived from stored repos",
	}
	cmd.AddCommand(
		newWorkspaceNewCmd(),
		newWorkspaceFromPRCmd(),
		newWorkspaceLsCmd(),
		newWorkspaceRmCmd(),
	)
	return cmd
}

func newWorkspaceNewCmd() *cobra.Command {
	var base string
	cmd := &cobra.Command{
		Use:   "new <repo> <name>",
		Short: "Create a workspace: a plain local clone off the repo's checkout",
		Long: `new creates a writable clone of the repo at
~/.shed/workspaces/<repo>/<name>/. The clone is purely local — objects
hardlink from shed's shared mirror — so creation is fast and works offline;
its origin is immediately re-pointed at the real upstream, so the workspace
is an ordinary git repo that pushes to GitHub like any clone.

<name> is the workspace's identity: it is the directory shed owns, it must be
unique across every repo (so 'shed resume <name>' is unambiguous), and it
seeds an initial git branch of the same name. The git branch is then yours to
rename or switch — shed keys on the workspace name, not the live branch.

If a branch named <name> exists on origin, checks it out. Otherwise creates
it off the repo's tracked branch or tag (or --base). shed always attempts a
sync first so the workspace forks from the freshest code; if that fails
(offline, auth, upstream down) it warns and forks from the last synced state.

In a terminal, progress lines ("syncing <repo>...DONE", "Creating
workspace: <path>") narrate the steps on stderr. When output is piped or captured,
the bare workspace path is printed on stdout, so 'cd "$(shed workspace new
<repo> <name>)"' works.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceNew(args[0], args[1], base)
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "branch or tag to fork from when <name> is new (default: the repo's track)")
	return cmd
}

func runWorkspaceNew(name, branch, base string) error {
	if err := gitx.RequireGit(); err != nil {
		return errs.Wrap(errs.MissingDep, err)
	}
	// Reject an unsafe branch/base up front, before the (network) sync below,
	// so a traversing or option-looking ref fails fast with a clear message.
	// workspace.New re-checks as the authoritative library-level guard.
	if err := paths.ValidateBranch(branch); err != nil {
		return errs.Wrap(errs.Config, err)
	}
	if base != "" {
		if err := paths.ValidateBranch(base); err != nil {
			return errs.Wrap(errs.Config, err)
		}
	}
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	repo, err := c.Resolve(name)
	if err != nil {
		return err
	}
	name, err = repo.ResolvedName()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	if err := guardNewWorkspace(c, name, branch); err != nil {
		return err
	}
	if _, err := syncForWorkspace(name, repo); err != nil {
		return err
	}
	path, err := createWorkspace(repo, name, branch, base)
	if err != nil {
		return err
	}
	// Best-effort: link this workspace to the agent session that created it, so
	// `shed resume <name>` can reopen it. The session comes from the
	// SHED_SESSION_* env override (headless) or the pending intent the pre-exec
	// hook recorded. A failure here never fails the create — resume is just
	// unavailable for an unlinked workspace.
	finalizeSessionLink(name, branch)
	emitWorkspacePath(path)
	return nil
}

// createWorkspace is the creation step shared by `workspace new` and
// `workspace from-pr`: resolve the workspace source, narrate on stderr, run
// workspace.New, and map its failures onto shed's exit codes. Returns the
// absolute workspace path.
func createWorkspace(repo *config.Repo, name, branch, base string) (string, error) {
	src, err := workspaceSource(repo, name)
	if err != nil {
		return "", errs.Wrap(errs.Config, err)
	}
	fmt.Fprintf(os.Stderr, "Creating workspace: %s\n", workspace.PathFor(name, branch))
	path, warnings, err := workspace.New(src, branch, base)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if err != nil {
		if errors.Is(err, mirror.ErrLocked) {
			return "", errs.Wrap(errs.Locked, err)
		}
		return "", errs.Wrap(errs.Network, err)
	}
	return path, nil
}

// emitWorkspacePath prints the bare workspace path on stdout when it is being
// piped or captured — the machine-readable contract that makes
// `cd "$(shed ws new …)"` and agent capture work. Interactive terminals
// already saw the "Creating workspace:" line, so nothing is printed there.
func emitWorkspacePath(path string) {
	if !isTerminal(os.Stdout) {
		fmt.Println(path)
	}
}

// guardNewWorkspace runs the pre-creation checks shared by `workspace new`
// and `workspace from-pr`: the branch must not name an existing workspace
// (half-created leftovers are repaired later, inside workspace.New), must not
// nest inside another workspace's tree, and must be unique across every repo
// and against repo names — so `shed resume <name>` and `shed path <name>`
// stay unambiguous. Runs before the network sync so a doomed name fails fast.
func guardNewWorkspace(c *config.Config, name, branch string) error {
	// A half-created leftover (crash between clone and `remote set-url`) is
	// repaired by replacement inside workspace.New; only a real workspace is
	// an "already exists" error.
	if workspace.Exists(name, branch) && !workspace.HalfCreated(name, branch) {
		return errs.New(errs.Exists, "workspace already exists at %s", workspace.PathFor(name, branch))
	}
	// A branch that is a path prefix of (or nested under) an existing
	// workspace's branch would clone inside that workspace's working tree,
	// where List can't see it and removing the enclosing workspace would delete
	// it. Refuse before the network sync; workspace.New re-checks authoritatively.
	if encl, ok := workspace.EnclosingWorkspace(name, branch); ok {
		return errs.New(errs.Exists,
			"workspace %q would be created inside existing workspace %s; pick a name that is not nested under another workspace",
			branch, encl)
	}
	// Workspace names are unique across the entire shed, so `shed resume <name>`
	// is unambiguous. Reject a name already taken under a *different* repo.
	if other, _, found := workspace.LocateByName(repoNames(c), branch); found && other != name {
		return errs.New(errs.Exists,
			"workspace name %q already exists for repo %s; pick a distinct name", branch, other)
	}
	// Workspace and repo names share one namespace so `shed path <name>` is
	// unambiguous. Reject a workspace name that would resolve to a repo.
	if repos := repoNamesMatching(c, branch); len(repos) > 0 {
		return errs.New(errs.Exists,
			"workspace name %q collides with repo %s; pick a distinct name so `shed path %s` is unambiguous",
			branch, repos[0], branch)
	}
	return nil
}

// syncForWorkspace is the pre-creation sync every workspace-creating command
// runs: ALWAYS attempt a sync first (mirror fetch + catalog fast-forward) so
// the workspace forks from the freshest code. If that fails but a synced repo
// already exists, warn and fall back to the last synced state — graceful
// degradation, so creation still works offline; only hard-fail when there is
// nothing on disk to fork from. refreshed reports whether the mirror really
// was brought up to date — false means the fallback path, which from-pr uses
// to avoid trusting a possibly-stale branch tip.
func syncForWorkspace(name string, repo *config.Repo) (refreshed bool, err error) {
	// A single repo refresh, so streaming git's progress meter can't
	// interleave; show it when interactive (nil when output is piped — see
	// isTerminal). Both modes end with one "syncing <repo>...DONE" (or
	// FAILED) line: quiet mode prints the header up front and completes it in
	// place, while interactive mode lets git's meter narrate first — a
	// leading header would either be clobbered by the meter's \r redraws or
	// need repeating — and states the labeled verdict after.
	var progress io.Writer
	if isTerminal(os.Stderr) {
		progress = os.Stderr
	} else {
		fmt.Fprintf(os.Stderr, "syncing %s...", name)
	}
	res := syncSingle(syncTarget{name, repo.URL, repo.Track, repo.Git}, progress)
	syncFailed := res.Status == "error" || res.Status == "gone"
	verdict := "DONE"
	if syncFailed {
		verdict = "FAILED"
	}
	if progress != nil {
		fmt.Fprintf(os.Stderr, "syncing %s...%s\n", name, verdict)
	} else {
		fmt.Fprintln(os.Stderr, verdict)
	}
	if syncFailed {
		if !catalog.Valid(name) {
			if res.locked {
				return false, errs.New(errs.Locked, "could not sync %s: %s", name, res.Error)
			}
			return false, errs.New(errs.Network, "could not sync %s: %s", name, res.Error)
		}
		fmt.Fprintf(os.Stderr, "warning: could not refresh %s (%s); using the last synced state\n", name, res.Error)
		return false, nil
	}
	return true, nil
}

// workspaceSource assembles the workspace.Source for a config repo: its
// catalog name, mirror identity, and the resolved track the catalog checks
// out (the default base for new branches). Resolution is purely local —
// against the mirror's already-fetched refs — so it works offline.
func workspaceSource(repo *config.Repo, name string) (workspace.Source, error) {
	key, err := repo.MirrorKey()
	if err != nil {
		return workspace.Source{}, err
	}
	ref, err := catalog.ResolveTrack(key, repo.Track)
	if err != nil {
		return workspace.Source{}, err
	}
	return workspace.Source{
		Repo:      name,
		MirrorKey: key,
		Track:     ref.Short,
		URL:       repo.URL,
		Git:       repo.Git,
	}, nil
}

// repoNames returns the resolved names of every repo in the config, skipping
// any that fail to resolve.
func repoNames(c *config.Config) []string {
	names := make([]string, 0, len(c.Repos))
	for _, r := range c.Repos {
		if n, err := r.ResolvedName(); err == nil {
			names = append(names, n)
		}
	}
	return names
}

// finalizeSessionLink writes the session-link sidecar for a just-created
// workspace, sourcing the session from the SHED_SESSION_* env override or the
// pending intent recorded by the pre-exec hook. The intent is looked up under
// pendingKeys — exactly the keys the hook may have recorded for this
// invocation — defaulting to the workspace name (the `workspace new` case;
// from-pr passes its own rendezvous key, see prPendingKey). Checking only
// this invocation's keys means a stale intent left by some other failed
// command can never be cross-wired onto this workspace. Consumed pending
// intents are cleared in passing. Best-effort: any problem (no session info,
// write error) leaves the workspace unlinked.
func finalizeSessionLink(repo, wsName string, pendingKeys ...string) {
	link, ok := sessionFromEnv()
	if !ok {
		if len(pendingKeys) == 0 {
			pendingKeys = []string{wsName}
		}
		var found *workspace.SessionLink
		for _, key := range pendingKeys {
			if p, err := workspace.TakePending(key); err == nil && p != nil {
				found = p
				break
			}
		}
		if found == nil {
			return
		}
		link = *found
	}
	if link.CWD == "" {
		if cwd, err := os.Getwd(); err == nil {
			link.CWD = cwd
		}
	}
	if link.LinkedAt.IsZero() {
		link.LinkedAt = time.Now().UTC()
	}
	_ = workspace.WriteLink(repo, wsName, link)
}

// sessionFromEnv reads a SHED_SESSION_* override. Both id and agent are
// required; cwd is optional (defaulted by the caller). Returns ok=false when no
// override is present.
func sessionFromEnv() (workspace.SessionLink, bool) {
	id := os.Getenv("SHED_SESSION_ID")
	agent := os.Getenv("SHED_SESSION_AGENT")
	if id == "" || agent == "" {
		return workspace.SessionLink{}, false
	}
	return workspace.SessionLink{
		Agent:     agent,
		SessionID: id,
		CWD:       os.Getenv("SHED_SESSION_CWD"),
	}, true
}

func newWorkspaceLsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List workspaces with dirty/unpushed state and age",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceList(jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a human-readable table")
	return cmd
}

func runWorkspaceList(jsonOut bool) error {
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	infos, err := collectWorkspaces(c)
	if err != nil {
		return err
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(infos)
	}
	if len(infos) == 0 {
		fmt.Println("(no workspaces)")
		return nil
	}
	return writeWorkspaceListTable(os.Stdout, infos)
}

// writeWorkspaceListTable renders the `workspace ls` table most-recently-active
// first, so the workspace you just touched sits at the top — matching the
// Workspaces section of `shed ls`. It sorts by the same ACTIVE column the table
// shows via the shared sortWorkspacesByAge helper.
func writeWorkspaceListTable(out io.Writer, infos []workspace.Info) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREPO\tDIRTY\tUNPUSHED\tACTIVE")
	for _, i := range sortWorkspacesByAge(infos) {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			i.Branch, i.Name, dirtyLabel(i.Dirty), unpushedLabel(i.Unpushed), relTime(i.Age))
	}
	return w.Flush()
}

func newWorkspaceRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <name>...",
		Short: "Delete one or more workspaces by name (refuses if dirty or unpushed unless --force)",
		Long: `rm deletes the workspaces with the given names.

Workspace names are unique across every repo (enforced at creation), so a
name alone identifies exactly one workspace — no <repo> is needed.

Several names may be given at once (e.g. "shed ws rm a b c"); each is removed
independently, so a failure on one (a typo, or a workspace with unsaved work)
is reported but does not stop the rest from being removed.

Refuses to delete a workspace with uncommitted or unpushed changes unless
--force is given.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceRmMany(args, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "delete even if there are uncommitted or unpushed changes")
	return cmd
}

// runWorkspaceRmMany removes each named workspace in turn. Each name is handled
// independently: a failure on one (a typo, or a workspace with unsaved work) is
// reported to stderr but does not stop the rest from being removed, so
// `shed ws rm a b c` removes whatever it can.
//
// A single name behaves exactly as a plain removal: its error propagates
// untouched to main for the "error:" line and the matching exit code. Duplicate
// names are collapsed so the same workspace isn't removed (and then reported as
// already-gone) twice.
func runWorkspaceRmMany(names []string, force bool) error {
	names = dedupeStrings(names)
	if len(names) == 1 {
		return runWorkspaceRm(names[0], force)
	}
	var failed []error
	for _, name := range names {
		if err := runWorkspaceRm(name, force); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			failed = append(failed, err)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	// The per-name errors were already printed above; return a concise summary
	// so main adds one closing line and exits non-zero. The exit code is the
	// shared code when every failure used the same one, else the generic config
	// code (the batch failed for mixed reasons).
	return errs.New(rmBatchExitCode(failed), "%s could not be removed", pluralize(len(failed), "workspace"))
}

func runWorkspaceRm(name string, force bool) error {
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	// Workspace names are globally unique, so the name alone locates exactly one
	// workspace (same lookup `shed resume` uses).
	repo, path, found := workspace.LocateByName(repoNames(c), name)
	if !found {
		return errs.New(errs.NotFound, "no workspace named %q (see `shed ls`)", name)
	}
	if !force {
		dirty, unpushed, err := workspace.CheckClean(path)
		if err != nil {
			return errs.Wrap(errs.Config, err)
		}
		if dirty || unpushed > 0 {
			parts := []string{}
			if dirty {
				parts = append(parts, "uncommitted changes")
			}
			if unpushed > 0 {
				parts = append(parts, fmt.Sprintf("%d unpushed commits", unpushed))
			}
			return errs.New(errs.Dirty,
				"workspace has %s; commit and push, or pass --force to discard",
				joinAnd(parts))
		}
	}
	if err := workspace.Remove(repo, name); err != nil {
		return errs.Wrap(errs.Config, err)
	}
	fmt.Printf("removed %s\n", paths.Display(path))
	return nil
}
