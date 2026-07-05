package workspace

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/AndrewHannigan/shed/pkg/gitx"
)

// CheckoutPRHead moves the workspace's current branch to a pull request's
// head commit: it fetches refs/pull/<number>/head from origin (the one ref
// GitHub publishes for every PR, fork or not — this is the network step) and
// hard-resets the branch to it. The branch keeps its name; only its tip
// moves. Any upstream tracking the branch picked up at creation is cleared,
// because the branch now holds PR commits, not the base branch it happened
// to be created from — a stale @{u} would make `workspace ls` report
// meaningless unpushed counts.
//
// The workspace is freshly created when this runs, so a hard reset can't
// discard work. Returns non-fatal warnings (LFS blobs unavailable), matching
// New's degradation: an unreachable LFS server leaves pointer stubs, never a
// failed (and deleted) workspace.
func CheckoutPRHead(path string, number int) (warnings []string, err error) {
	if number <= 0 {
		return nil, fmt.Errorf("invalid PR number %d", number)
	}
	// The refspec is formatted from an int, so it can never read as a git
	// option. No local ref is created: FETCH_HEAD is enough for the reset,
	// and refs/pull/* stays an upstream-owned namespace.
	if err := gitx.RunEnv(path, lfsSkip, "fetch", "origin", fmt.Sprintf("refs/pull/%d/head", number)); err != nil {
		return nil, fmt.Errorf("fetch PR head: %w", err)
	}
	// lfsSkip on the reset too: checking out the PR's files must not invoke
	// the LFS smudge filter, whose network failure would fail the reset (New
	// treats the same situation as a pointer-stubs warning). Blobs are
	// resolved best-effort below.
	if err := gitx.RunEnv(path, lfsSkip, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return nil, fmt.Errorf("reset to PR head: %w", err)
	}
	// The branch may or may not have tracking (created fresh: none; named
	// after an existing branch: origin/<name>) — unsetting when there is none
	// exits non-zero, which is fine to ignore.
	_ = gitx.Run(path, "branch", "--unset-upstream")
	if usesLFS(path) {
		if _, lfsErr := exec.LookPath("git-lfs"); lfsErr != nil {
			warnings = append(warnings, "repo uses git LFS but git-lfs is not installed; files are pointer stubs")
		} else if err := gitx.Run(path, "lfs", "pull"); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not fetch LFS objects (offline?); files are pointer stubs: %v", err))
		}
	}
	return warnings, nil
}

// AddForkRemote wires a cross-repo PR workspace for pushing back to the
// contributor's fork: it adds a remote named "fork" pointing at url, and —
// when trackRef is non-empty — fetches that branch from the fork and sets it
// as the current branch's upstream, so a plain `git push` updates the PR.
//
// The tracking half is best-effort: a fork that can't be fetched (deleted,
// private, offline) leaves the remote in place and reports tracked=false, so
// the caller can print how to push manually instead of failing a workspace
// that is already fully usable for review.
func AddForkRemote(path, url, trackRef string) (tracked bool, err error) {
	if url == "" || strings.HasPrefix(url, "-") {
		return false, fmt.Errorf("invalid fork remote URL %q", url)
	}
	if err := gitx.Run(path, "remote", "add", "fork", url); err != nil {
		return false, fmt.Errorf("add fork remote: %w", err)
	}
	if trackRef == "" {
		return false, nil
	}
	// trackRef was validated as a safe branch name by the caller (it comes
	// from planPRCheckout, which only sets it to an already-validated name).
	if err := gitx.RunEnv(path, lfsSkip, "fetch", "fork", trackRef); err != nil {
		return false, nil
	}
	if err := gitx.Run(path, "branch", "--set-upstream-to=fork/"+trackRef); err != nil {
		return false, nil
	}
	return true, nil
}
