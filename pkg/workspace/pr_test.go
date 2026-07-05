package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prTestRepos builds the shape from-pr works against: an upstream whose main
// has one commit, plus a PR head commit reachable only via refs/pull/7/head
// (exactly how GitHub publishes PR heads), and a workspace-like clone of the
// upstream sitting on a fresh branch. Returns (upstream, clone, prSHA).
func prTestRepos(t *testing.T) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()

	upstream := filepath.Join(root, "upstream")
	git(t, root, nil, "init", "-q", "-b", "main", upstream)
	writeFile(t, upstream, "a.txt", "1")
	git(t, upstream, nil, "add", "a.txt")
	git(t, upstream, nil, "commit", "-q", "-m", "base")

	// The PR's commit lives on a temporary branch, is captured under
	// refs/pull/7/head, and the branch is deleted — so the commit is
	// reachable only the way a real PR head is.
	git(t, upstream, nil, "checkout", "-q", "-b", "pr-work")
	writeFile(t, upstream, "fix.txt", "fixed")
	git(t, upstream, nil, "add", "fix.txt")
	git(t, upstream, nil, "commit", "-q", "-m", "the fix")
	prSHA := revParse(t, upstream, "HEAD")
	git(t, upstream, nil, "update-ref", "refs/pull/7/head", prSHA)
	git(t, upstream, nil, "checkout", "-q", "main")
	git(t, upstream, nil, "branch", "-q", "-D", "pr-work")

	clone := filepath.Join(root, "ws")
	git(t, root, nil, "clone", "-q", upstream, clone)
	git(t, clone, nil, "checkout", "-q", "-b", "review-pr-7")
	return upstream, clone, prSHA
}

func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", rev).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}

func TestCheckoutPRHead(t *testing.T) {
	_, ws, prSHA := prTestRepos(t)

	if err := CheckoutPRHead(ws, 7); err != nil {
		t.Fatalf("CheckoutPRHead: %v", err)
	}
	if got := revParse(t, ws, "HEAD"); got != prSHA {
		t.Fatalf("HEAD = %s, want PR head %s", got, prSHA)
	}
	// Still on the same branch (reset moves the tip, not HEAD's ref)...
	if br, err := CurrentBranch(ws); err != nil || br != "review-pr-7" {
		t.Fatalf("CurrentBranch = %q, %v; want review-pr-7", br, err)
	}
	// ...the PR's file is in the tree...
	if _, err := os.Stat(filepath.Join(ws, "fix.txt")); err != nil {
		t.Fatalf("PR file missing after checkout: %v", err)
	}
	// ...and no stale upstream tracking survives.
	if out, err := exec.Command("git", "-C", ws, "rev-parse", "--abbrev-ref", "@{u}").Output(); err == nil {
		t.Fatalf("branch still has upstream %s; want none", strings.TrimSpace(string(out)))
	}
}

// A branch created with tracking (like a workspace named after an existing
// upstream branch) must lose that tracking when its tip becomes a PR head.
func TestCheckoutPRHeadClearsTracking(t *testing.T) {
	_, ws, _ := prTestRepos(t)
	git(t, ws, nil, "branch", "-q", "--set-upstream-to=origin/main")

	if err := CheckoutPRHead(ws, 7); err != nil {
		t.Fatalf("CheckoutPRHead: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "rev-parse", "--abbrev-ref", "@{u}").Output(); err == nil {
		t.Fatalf("branch still has upstream %s; want none", strings.TrimSpace(string(out)))
	}
}

func TestCheckoutPRHeadMissingPR(t *testing.T) {
	_, ws, _ := prTestRepos(t)
	if err := CheckoutPRHead(ws, 999); err == nil {
		t.Fatal("CheckoutPRHead(999) should fail: no such pull ref upstream")
	}
	if err := CheckoutPRHead(ws, 0); err == nil {
		t.Fatal("CheckoutPRHead(0) should fail fast on an invalid number")
	}
}

func TestAddForkRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()

	// The "fork": a repo whose branch fix-1 holds the PR commit.
	fork := filepath.Join(root, "fork")
	git(t, root, nil, "init", "-q", "-b", "main", fork)
	writeFile(t, fork, "a.txt", "1")
	git(t, fork, nil, "add", "a.txt")
	git(t, fork, nil, "commit", "-q", "-m", "base")
	git(t, fork, nil, "checkout", "-q", "-b", "fix-1")
	writeFile(t, fork, "fix.txt", "fixed")
	git(t, fork, nil, "add", "fix.txt")
	git(t, fork, nil, "commit", "-q", "-m", "fix")
	forkSHA := revParse(t, fork, "HEAD")

	// The workspace: a clone of the fork's main, on a branch named after the
	// PR head, already reset to the PR commit (what CheckoutPRHead leaves).
	ws := filepath.Join(root, "ws")
	git(t, root, nil, "clone", "-q", "--branch", "main", fork, ws)
	git(t, ws, nil, "checkout", "-q", "-b", "fix-1")
	git(t, ws, nil, "fetch", "-q", "origin", "refs/heads/fix-1")
	git(t, ws, nil, "reset", "-q", "--hard", "FETCH_HEAD")

	tracked, err := AddForkRemote(ws, fork, "fix-1")
	if err != nil {
		t.Fatalf("AddForkRemote: %v", err)
	}
	if !tracked {
		t.Fatal("AddForkRemote reported tracked=false for a reachable fork")
	}
	if url, err := exec.Command("git", "-C", ws, "remote", "get-url", "fork").Output(); err != nil || strings.TrimSpace(string(url)) != fork {
		t.Fatalf("fork remote = %q, %v; want %q", strings.TrimSpace(string(url)), err, fork)
	}
	out, err := exec.Command("git", "-C", ws, "rev-parse", "--abbrev-ref", "@{u}").Output()
	if err != nil || strings.TrimSpace(string(out)) != "fork/fix-1" {
		t.Fatalf("upstream = %q, %v; want fork/fix-1", strings.TrimSpace(string(out)), err)
	}
	// The tracked state is coherent: no unpushed commits against the fork.
	if got := revParse(t, ws, "fork/fix-1"); got != forkSHA {
		t.Fatalf("fork/fix-1 = %s, want %s", got, forkSHA)
	}

	// An unreachable fork keeps the remote but degrades to tracked=false.
	ws2 := filepath.Join(root, "ws2")
	git(t, root, nil, "clone", "-q", fork, ws2)
	tracked, err = AddForkRemote(ws2, filepath.Join(root, "gone"), "fix-1")
	if err != nil {
		t.Fatalf("AddForkRemote (unreachable): %v", err)
	}
	if tracked {
		t.Fatal("AddForkRemote reported tracked=true for an unreachable fork")
	}
	if err := exec.Command("git", "-C", ws2, "remote", "get-url", "fork").Run(); err != nil {
		t.Fatal("fork remote should still be configured for manual pushes")
	}

	// A hostile URL is rejected before any git runs.
	if _, err := AddForkRemote(ws2, "--upload-pack=evil", "x"); err == nil {
		t.Fatal("AddForkRemote should reject an option-shaped URL")
	}
}
