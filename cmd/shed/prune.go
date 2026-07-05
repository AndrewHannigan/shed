package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/catalog"
	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/forge"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

func newPruneCmd() *cobra.Command {
	var dryRun, force, yes bool
	var ifOlderThan time.Duration
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete workspaces whose work has already landed",
		Long: `prune removes every workspace whose work has already landed, reclaiming
the ones that are safe to delete. A workspace is reclaimed when the branch
checked out in it (which may have been created or renamed since the workspace
was made, so this need not be the workspace's name) has a merged pull
request, or its own commits are already contained in the remote default
branch (a merge- or rebase-merge with no PR). The merged-PR
check asks GitHub via the gh CLI, so gh must be installed and authenticated.

A workspace that never committed anything of its own is kept, even though its
tip already sits on the default branch: an empty workspace has nothing to
reclaim, so "no commits beyond the default branch" is not on its own a reason
to delete it.

With --if-older-than, also reclaim workspaces whose last activity (newest
reflog entry) is older than the given duration, regardless of merge status.

Workspaces with uncommitted or unpushed changes are skipped so local work is
never lost; pass --force to remove them anyway. Before deleting, prune lists
the workspaces and asks for confirmation; pass --yes to skip the prompt or
--dry-run to preview without deleting.

prune is also where shed does its own upkeep, so you never run git
maintenance by hand: leftover repo checkouts that no longer match the config
(e.g. after changing a repo's track) are removed, each mirror is compacted
(git gc — after workspace removal, so less old pack data stays pinned), and
mirrors that no config entry references anymore are deleted — nothing else
ever deletes a mirror.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrune(dryRun, force, yes, ifOlderThan)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be removed without deleting")
	cmd.Flags().BoolVar(&force, "force", false, "remove even if there are uncommitted or unpushed changes")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().DurationVar(&ifOlderThan, "if-older-than", 0, "also remove workspaces inactive longer than this (e.g. 720h)")
	return cmd
}

// prunePlan is a workspace prune has decided to delete, with the reason for it.
type prunePlan struct {
	info   workspace.Info
	reason string
}

func runPrune(dryRun, force, yes bool, ifOlderThan time.Duration) error {
	if err := gitx.RequireGit(); err != nil {
		return errs.Wrap(errs.MissingDep, err)
	}
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	infos, err := workspace.List(repoNames(c))
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	if len(infos) == 0 {
		fmt.Println("(no workspaces)")
		return pruneMaintenance(c, dryRun)
	}
	// prune leans on gh for the merged-PR check, so fail fast (rather than
	// degrade) when gh can't tell us which branches are merged. Checked only
	// once workspaces exist — the maintenance-only path above never needs gh.
	if err := forge.Available(); err != nil {
		if errors.Is(err, forge.ErrGhMissing) {
			return errs.Wrap(errs.MissingDep, err)
		}
		return errs.Wrap(errs.Network, err)
	}

	now := time.Now()
	var plans []prunePlan
	var skipped, kept, failed int
	for _, i := range infos {
		// The gh slug comes from the repo's upstream identity, not its catalog
		// name: a track-pinned repo's name carries an "@<track>" suffix that
		// gh would reject as an invalid OWNER/REPO.
		slugSource := i.Name
		if r := c.FindByName(i.Name); r != nil {
			if k, err := r.MirrorKey(); err == nil {
				slugSource = k
			}
		}
		host, repo, ok := ghRepoFromName(slugSource)
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: cannot derive a GitHub repo from %q\n", i.Branch, i.Name)
			failed++
			continue
		}
		// Ask GitHub about the branch checked out in the workspace, not the
		// directory name: the two start out identical, but work is often
		// pushed from a branch created or renamed inside the workspace, and a
		// PR merged from that branch must still reclaim it. Detached HEAD
		// falls back to the directory name.
		branch := i.Branch
		if cur, err := workspace.CurrentBranch(i.Path); err == nil {
			branch = cur
		}
		pr, err := forge.MergedPR(host, repo, branch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s %s: could not check PR status: %v\n", repo, branch, err)
			failed++
			continue
		}
		// Only consult git when there's no merged PR: a found PR is the
		// stronger signal and gives the clearer message, and the ancestor
		// check is redundant once we know it merged.
		var landed, hasOwnCommits bool
		var defaultBranch string
		if pr == 0 {
			landed, hasOwnCommits, defaultBranch, err = workspace.LandedInDefault(i.Path, branch)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s %s: could not check default-branch status: %v\n", repo, i.Branch, err)
			}
		}
		expired := ifOlderThan > 0 && !i.Age.IsZero() && now.Sub(i.Age) > ifOlderThan
		prunable := reclaimable(pr, landed, hasOwnCommits, expired)
		reason := pruneReason(pr, landed, hasOwnCommits, defaultBranch, expired, now.Sub(i.Age))
		switch decidePrune(prunable, i.Dirty, i.Unpushed, force) {
		case pruneKeep:
			kept++
		case pruneSkip:
			fmt.Printf("skipped %s (%s, but has %s)\n", i.Branch, reason, localChangesDesc(i))
			skipped++
		case pruneRemove:
			plans = append(plans, prunePlan{info: i, reason: reason})
		}
	}

	pruned := 0
	switch {
	case dryRun:
		for _, p := range plans {
			fmt.Printf("would prune %s (%s)\n", p.info.Branch, p.reason)
		}
		pruned = len(plans)
	case len(plans) == 0:
		// nothing to delete
	default:
		fmt.Printf("The following %s will be deleted:\n", countNoun(len(plans), "workspace"))
		for _, p := range plans {
			fmt.Printf("  %s (%s)\n", p.info.Branch, p.reason)
		}
		if !yes && !confirmDeletion() {
			fmt.Println("aborted")
			kept += len(plans)
			return nil
		}
		for _, p := range plans {
			if err := workspace.Remove(p.info.Name, p.info.Branch); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not remove %s: %v\n", p.info.Branch, err)
				failed++
				continue
			}
			fmt.Printf("pruned %s (%s)\n", p.info.Branch, p.reason)
			pruned++
		}
	}

	prunedLabel := "pruned"
	if dryRun {
		prunedLabel = "would prune"
	}
	parts := []string{fmt.Sprintf("%d %s", pruned, prunedLabel)}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	parts = append(parts, fmt.Sprintf("%d kept", kept))
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	fmt.Println(strings.Join(parts, ", "))
	return pruneMaintenance(c, dryRun)
}

// pruneMaintenanceLockTimeout bounds how long the maintenance phase waits for
// a mirror's exclusive lock; a busy mirror (sync or workspace creation in
// flight) is simply skipped this round.
const pruneMaintenanceLockTimeout = 5 * time.Second

// pruneMaintenance is the shed-owned upkeep pass, run after workspace
// removal, in order: (1) remove orphan repo checkouts no config entry claims
// (the leftover of a changed track — an identity change is remove-and-add);
// (2) gc each configured mirror under its exclusive lock — repacking after
// removal minimizes how much old pack data surviving hardlinked workspaces
// keep pinned — plus `git worktree prune` for stale bookkeeping; (3) remove
// mirrors that no config entry references — nothing else ever deletes a
// mirror. All best-effort: a failure is reported and the pass moves on.
func pruneMaintenance(c *config.Config, dryRun bool) error {
	known := make(map[string]bool, len(c.Repos))
	keys := make(map[string]*config.Repo)
	for i := range c.Repos {
		if n, err := c.Repos[i].ResolvedName(); err == nil {
			known[n] = true
		}
		if k, err := c.Repos[i].MirrorKey(); err == nil {
			keys[k] = &c.Repos[i]
		}
	}

	// (1) Orphan repo checkouts.
	if onDisk, err := catalog.OnDisk(); err == nil {
		for _, name := range onDisk {
			if known[name] {
				continue
			}
			if dryRun {
				fmt.Printf("would remove orphan repo checkout %s (not in config)\n", name)
				continue
			}
			// The mirror key is unknowable from the dir alone; catalog.Remove
			// falls back to a plain delete and each mirror's `worktree prune`
			// below clears the stale bookkeeping.
			if err := catalog.Remove("", name); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not remove orphan checkout %s: %v\n", name, err)
				continue
			}
			fmt.Printf("removed orphan repo checkout %s (not in config)\n", name)
		}
	}

	// (2) Mirror gc + worktree prune, (3) orphan-mirror removal. A crashed
	// removal leaves a renamed-aside tree nothing reclaims by key; sweep
	// those first.
	if !dryRun {
		mirror.RemoveRemnants()
	}
	onDisk, err := mirror.OnDisk()
	if err != nil {
		return nil
	}
	for _, key := range onDisk {
		if _, referenced := keys[key]; !referenced {
			if dryRun {
				fmt.Printf("would remove mirror %s (no config entry references it)\n", key)
				continue
			}
			if err := mirror.Remove(key, pruneMaintenanceLockTimeout); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not remove mirror %s: %v\n", key, err)
				continue
			}
			fmt.Printf("removed mirror %s (no config entry references it)\n", key)
			continue
		}
		if dryRun {
			continue
		}
		lock, err := mirror.AcquireLock(key, true, pruneMaintenanceLockTimeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping maintenance of mirror %s: %v\n", key, err)
			continue
		}
		if err := mirror.Gc(key); err != nil {
			fmt.Fprintf(os.Stderr, "warning: gc of mirror %s failed: %v\n", key, err)
		}
		if err := mirror.PruneWorktrees(key); err != nil {
			fmt.Fprintf(os.Stderr, "warning: worktree prune of mirror %s failed: %v\n", key, err)
		}
		lock.Unlock()
	}
	return nil
}

// reclaimable reports whether some condition marks a workspace's work as having
// landed (or otherwise safe to reclaim): a merged PR, its own commits already
// contained in the default branch, or age (expired).
//
// A branch that landed but never committed anything of its own (landed &&
// !hasOwnCommits) is deliberately NOT reclaimable on that basis: its tip merely
// sits on the default branch because a fresh workspace never diverged, and an
// empty workspace has nothing to reclaim — having no commits beyond the default
// branch is not, on its own, a reason to delete it. Age can still reclaim such a
// workspace via --if-older-than. Pure, so it is unit-testable.
func reclaimable(prNumber int, landed, hasOwnCommits, expired bool) bool {
	return prNumber != 0 || (landed && hasOwnCommits) || expired
}

// pruneAction is what prune decides to do with one workspace.
type pruneAction int

const (
	pruneKeep   pruneAction = iota // not reclaimable — leave it alone
	pruneSkip                      // reclaimable, but has local work and --force wasn't given
	pruneRemove                    // reclaimable and safe to delete
)

// decidePrune chooses an action for a workspace. prunable is true when some
// condition (merged PR, landed in the default branch, or age) marks the
// workspace as reclaimable. dirty flags uncommitted changes; unpushed is the
// unpushed-commit count (-1 when no upstream, treated as "nothing unpushed"
// since a landed branch reached the remote). Pure, so it is unit-testable.
func decidePrune(prunable, dirty bool, unpushed int, force bool) pruneAction {
	if !prunable {
		return pruneKeep
	}
	if !force && (dirty || unpushed > 0) {
		return pruneSkip
	}
	return pruneRemove
}

// pruneReason describes why a workspace is being pruned, for status messages.
// Reasons are reported in priority order: a merged PR is the clearest signal,
// then containment in the default branch, then age. A landed branch that never
// committed anything (hasOwnCommits is false) is not a prune reason on its own —
// an empty workspace has nothing to reclaim — so it falls through to age (or
// none), never to "merged".
func pruneReason(prNumber int, landed, hasOwnCommits bool, defaultBranch string, expired bool, inactive time.Duration) string {
	switch {
	case prNumber != 0:
		return fmt.Sprintf("PR #%d merged", prNumber)
	case landed && hasOwnCommits:
		if defaultBranch != "" {
			return fmt.Sprintf("merged into %s", defaultBranch)
		}
		return "merged into default branch"
	case expired:
		return fmt.Sprintf("inactive for %s", relDuration(inactive))
	default:
		return ""
	}
}

// confirmDeletion prompts on stderr for a yes/no before prune deletes. When
// stdin isn't a TTY it refuses rather than delete unattended, pointing at the
// --yes / --dry-run escape hatches.
func confirmDeletion() bool {
	if !stdinIsTTY() {
		fmt.Fprintln(os.Stderr, "refusing to delete without confirmation; re-run with --yes (or --dry-run to preview)")
		return false
	}
	fmt.Fprint(os.Stderr, "Delete these workspaces? [y/N] ")
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

// countNoun renders "1 workspace" / "3 workspaces" for human-readable counts.
func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// ghRepoFromName splits a workspace repo name ("host/owner/repo", possibly
// with an "@<track>" suffix on the leaf) into the GitHub host and the
// "owner/repo" slug gh expects. The suffix is trimmed as a fallback for
// workspaces whose config entry is gone — the caller prefers the entry's
// URL-derived identity when one exists. ok is false unless the name has a
// host plus an owner/repo path. Pure, so it is unit-testable.
func ghRepoFromName(name string) (host, repo string, ok bool) {
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	h, rest, found := strings.Cut(name, "/")
	if !found || h == "" || !strings.Contains(rest, "/") {
		return "", "", false
	}
	return h, rest, true
}

// localChangesDesc describes a workspace's uncommitted/unpushed state for the
// "skipped" message.
func localChangesDesc(i workspace.Info) string {
	parts := []string{}
	if i.Dirty {
		parts = append(parts, "uncommitted changes")
	}
	if i.Unpushed > 0 {
		parts = append(parts, fmt.Sprintf("%d unpushed commits", i.Unpushed))
	}
	return joinAnd(parts)
}
