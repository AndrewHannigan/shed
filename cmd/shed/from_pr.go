package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/forge"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

// prView is forge.ViewPR behind a seam so tests can simulate gh being
// missing, a same-repo PR, or a cross-repo (fork) PR without gh installed.
var prView = forge.ViewPR

func newWorkspaceFromPRCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "from-pr <pr>",
		Short: "Create a workspace checked out at a pull request's head",
		Long: `from-pr creates a workspace holding an existing pull request's branch, for
reviewing it or pushing fixes to it. <pr> may be a URL or a #-reference:

    shed workspace from-pr https://github.com/octocat/Hello-World/pull/42
    shed workspace from-pr octocat/Hello-World#42
    shed workspace from-pr Hello-World#42

The repo must already be in the library (see 'shed add'). Like 'workspace
new', creation forks off a freshly synced repo and the result is an ordinary
local clone with origin pointing at the real upstream.

The workspace is named after the PR's head branch (override with --name).
Metadata comes from the gh CLI when it is installed and authenticated:

  - A PR from a branch in the same repo checks that branch out, tracking
    origin, so a plain 'git push' updates the PR.
  - A PR from a fork fetches the PR head (refs/pull/<n>/head) from origin and
    adds a second remote named "fork" pointing at the contributor's fork;
    when the fork is reachable the branch tracks it, so 'git push' updates
    the PR there too.

Without gh, from-pr still works: the workspace is named pr-<number> and holds
the PR head fetched from origin, but nothing is wired for pushing back.

In a terminal, progress lines narrate the steps on stderr. When output is
piped or captured, the bare workspace path is printed on stdout, so
'cd "$(shed workspace from-pr <pr>)"' works.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceFromPR(args[0], name)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "workspace name (default: the PR's head branch)")
	return cmd
}

func runWorkspaceFromPR(prRef, nameFlag string) error {
	if err := gitx.RequireGit(); err != nil {
		return errs.Wrap(errs.MissingDep, err)
	}
	repoToken, number, err := parsePRRef(prRef)
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	if nameFlag != "" {
		if err := paths.ValidateBranch(nameFlag); err != nil {
			return errs.Wrap(errs.Config, err)
		}
	}
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	repo, err := c.Resolve(repoToken)
	if err != nil {
		// from-pr is often the very first touch of a repo (an agent handed a
		// PR URL), so a miss points at the fix rather than just "not found".
		var coded *errs.Coded
		if errors.As(err, &coded) && coded.Code == errs.NotFound {
			return errs.New(errs.NotFound,
				"repo %q is not in the library; add it first with `shed add %s`, then re-run",
				repoToken, addSuggestion(repoToken))
		}
		return err
	}
	name, err := repo.ResolvedName()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}

	// Ask GitHub about the PR (head branch, fork or not, open or not). gh
	// being missing or unauthenticated degrades — the PR head is still
	// reachable as refs/pull/<n>/head — but any other failure (no such PR,
	// network down) is a real error: proceeding would build a workspace off
	// the wrong commit or none at all.
	host, slug, ok := ghRepoFromName(name)
	if !ok {
		return errs.New(errs.Config, "cannot derive a GitHub owner/repo from %q", name)
	}
	info, ghErr := prView(host, slug, number)
	if ghErr != nil && !errors.Is(ghErr, forge.ErrGhMissing) && !errors.Is(ghErr, forge.ErrGhUnauthed) {
		return errs.Wrap(errs.Network, fmt.Errorf("could not look up PR #%d in %s: %w", number, slug, ghErr))
	}

	branch, warnings, err := fromPRBranch(info, ghErr, nameFlag, number)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if err != nil {
		return err
	}
	if err := guardNewWorkspace(c, name, branch); err != nil {
		return err
	}
	if err := syncForWorkspace(name, repo); err != nil {
		return err
	}

	// The checkout strategy needs the freshly synced mirror: a same-repo PR
	// whose head branch made it into the mirror is a plain branch checkout
	// (offline from here on); everything else goes through refs/pull/<n>/head.
	mirrorHasHead := false
	if ghErr == nil && !info.CrossRepo && info.HeadRefName != "" {
		if key, err := repo.MirrorKey(); err == nil {
			mirrorHasHead, _ = gitx.RefExists(paths.MirrorPath(key), "refs/remotes/origin/"+info.HeadRefName)
		}
	}
	co := planPRCheckout(info, ghErr, mirrorHasHead, branch, repo.URL, host)

	src, err := workspaceSource(repo, name)
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	fmt.Fprintf(os.Stderr, "Creating workspace: %s\n", workspace.PathFor(name, branch))
	path, warns, err := workspace.New(src, branch, co.base)
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if err != nil {
		if errors.Is(err, mirror.ErrLocked) {
			return errs.Wrap(errs.Locked, err)
		}
		return errs.Wrap(errs.Network, err)
	}
	if co.pullFetch {
		fmt.Fprintf(os.Stderr, "Fetching PR #%d head from origin\n", number)
		if err := workspace.CheckoutPRHead(path, number); err != nil {
			// Without the PR's commits the workspace is a trap — a branch off
			// the default tip that looks like the PR. Remove it so a retry
			// starts clean.
			os.RemoveAll(path)
			return errs.Wrap(errs.Network,
				fmt.Errorf("could not fetch PR #%d from origin: %w", number, err))
		}
	}
	if co.forkURL != "" {
		if tracked, err := workspace.AddForkRemote(path, co.forkURL, co.trackRef); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not add fork remote %s: %v\n", co.forkURL, err)
		} else if tracked {
			fmt.Fprintf(os.Stderr, "PR head is a fork; added remote \"fork\" (%s) — `git push` updates the PR\n", co.forkURL)
		} else {
			fmt.Fprintf(os.Stderr, "PR head is a fork; added remote \"fork\" (%s) — push back with `git push fork HEAD:%s`\n",
				co.forkURL, pushRefHint(info))
		}
	}
	// The pre-exec hook records the pending session intent under "pr-<n>"
	// when the from-pr command carries no --name (it can't know the head
	// branch); check that key too.
	finalizeSessionLink(name, branch, fmt.Sprintf("pr-%d", number))
	if !isTerminal(os.Stdout) {
		fmt.Println(path)
	}
	return nil
}

// parsePRRef parses the <pr> argument into a repo token (resolved against the
// config like any other repo name) and a PR number. Accepted forms:
//
//	https://github.com/OWNER/REPO/pull/123[/files...]  → "github.com/OWNER/REPO", 123
//	OWNER/REPO#123                                     → "OWNER/REPO", 123
//	REPO#123                                           → "REPO", 123
//
// Pure, so it is unit-testable.
func parsePRRef(s string) (repoToken string, number int, err error) {
	if strings.Contains(s, "://") || strings.HasPrefix(s, "git@") {
		host, path, perr := paths.ParseURL(s)
		if perr != nil {
			return "", 0, perr
		}
		segs := strings.Split(path, "/")
		if len(segs) < 4 || segs[2] != "pull" {
			return "", 0, fmt.Errorf("%q is not a pull request URL (want .../OWNER/REPO/pull/NUMBER)", s)
		}
		n, aerr := strconv.Atoi(segs[3])
		if aerr != nil || n <= 0 {
			return "", 0, fmt.Errorf("%q has no PR number after /pull/", s)
		}
		return host + "/" + segs[0] + "/" + segs[1], n, nil
	}
	repoToken, numStr, found := strings.Cut(s, "#")
	if !found || repoToken == "" {
		return "", 0, fmt.Errorf("%q is not a PR reference (want a PR URL or <repo>#<number>)", s)
	}
	n, aerr := strconv.Atoi(numStr)
	if aerr != nil || n <= 0 {
		return "", 0, fmt.Errorf("%q has no PR number after #", s)
	}
	return repoToken, n, nil
}

// addSuggestion turns a from-pr repo token into what the user would pass to
// `shed add`: github.com URLs shorten to gh shorthand ("owner/repo"), other
// hosts keep the full token. Pure, so it is unit-testable.
func addSuggestion(token string) string {
	if rest, ok := strings.CutPrefix(token, "github.com/"); ok {
		return rest
	}
	return token
}

// fromPRBranch picks the workspace's name (= its initial git branch): the
// --name override, else the PR's head branch, else pr-<number> when gh
// couldn't say. Returned warnings narrate the degraded paths. Pure, so it is
// unit-testable.
func fromPRBranch(info forge.PR, ghErr error, nameFlag string, number int) (string, []string, error) {
	var warnings []string
	if ghErr != nil {
		warnings = append(warnings,
			fmt.Sprintf("%v — created from refs/pull/%d/head without PR metadata; nothing is wired for pushing back", ghErr, number))
	} else if info.State != "" && info.State != "OPEN" {
		warnings = append(warnings,
			fmt.Sprintf("PR #%d is %s; the workspace will hold its final state", number, strings.ToLower(info.State)))
	}
	if nameFlag != "" {
		return nameFlag, warnings, nil
	}
	if ghErr != nil || info.HeadRefName == "" {
		return fmt.Sprintf("pr-%d", number), warnings, nil
	}
	// The head branch becomes a directory under workspaces/ and a git branch,
	// so it must pass the same safety check as a user-chosen name.
	if err := paths.ValidateBranch(info.HeadRefName); err != nil {
		return "", warnings, errs.New(errs.Config,
			"PR head branch cannot be used as a workspace name (%v); pick one with --name", err)
	}
	return info.HeadRefName, warnings, nil
}

// prCheckout is how runWorkspaceFromPR realizes the PR's head commit in the
// new workspace.
type prCheckout struct {
	base      string // base ref for workspace.New ("" = the repo's track)
	pullFetch bool   // fetch refs/pull/<n>/head from origin and hard-reset to it
	forkURL   string // non-empty: add a "fork" remote for pushing back
	trackRef  string // fork branch the workspace branch should track ("" = none)
}

// planPRCheckout decides the checkout strategy after the sync. The plain path
// — the workspace branch IS the PR's head branch, present in the mirror —
// needs no extra fetch: workspace.New checks it out with origin tracking, so
// `git push` updates the PR. Everything else (a fork PR, a deleted or
// not-yet-synced head branch, no gh, a --name override) goes through
// refs/pull/<n>/head, which exists for every PR regardless; the override case
// takes it too because workspace.New prefers an existing upstream branch of
// the same name over any base, and the hard reset makes the tip right no
// matter what the clone checked out. Pure, so it is unit-testable.
func planPRCheckout(info forge.PR, ghErr error, mirrorHasHead bool, branch, repoURL, host string) prCheckout {
	if ghErr == nil && !info.CrossRepo && mirrorHasHead && branch == info.HeadRefName {
		return prCheckout{}
	}
	co := prCheckout{pullFetch: true}
	if ghErr == nil && info.CrossRepo && info.HeadOwner != "" && info.HeadName != "" {
		co.forkURL = forge.ForkCloneURL(repoURL, host, info.HeadOwner, info.HeadName)
		// Tracking only helps when the branch shares the fork branch's name —
		// otherwise `git push` would refuse the mismatch and the tracked
		// upstream would just confuse; the push hint covers that case.
		if branch == info.HeadRefName && paths.ValidateBranch(info.HeadRefName) == nil {
			co.trackRef = info.HeadRefName
		}
	}
	return co
}

// pushRefHint names the ref to push to on the fork: the PR's head branch when
// known, else a placeholder.
func pushRefHint(info forge.PR) string {
	if info.HeadRefName != "" {
		return info.HeadRefName
	}
	return "<head-branch>"
}
