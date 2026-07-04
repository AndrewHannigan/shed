// Package mirror manages the fetch-only mirror repos under
// ~/.shed/.internal/mirrors: one per unique upstream, shared by every catalog
// repo tracking that upstream, and the only place shed talks to the network.
//
// A mirror is a normal (non-bare) repo whose working tree is never checked
// out and whose HEAD is detached — it must not occupy any branch, since
// refs/heads/* belongs to the catalog worktrees. It fetches with the standard
// clone refspec plus a forced tag refspec:
//
//	fetch = +refs/heads/*:refs/remotes/origin/*
//	fetch = +refs/tags/*:refs/tags/*
//
// Upstream truth lands in refs/remotes/origin/*, which no worktree can have
// checked out — so a fetch is structurally unblockable no matter what state
// an agent leaves a catalog in. The mirror's local branch namespace
// (refs/heads/*) holds exactly one branch per branch-tracked catalog repo.
//
// Only shed writes to mirrors; agents only ever write to workspaces.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// ErrLocked is returned when a mirror's lock cannot be acquired in time.
var ErrLocked = errors.New("mirror locked by another process")

// Exists reports whether the mirror is present on disk (its .git dir exists —
// a bare top-level dir left by some interrupted operation does not count, so
// a half-mirror is never trusted).
func Exists(key string) bool {
	s, err := os.Stat(filepath.Join(paths.MirrorPath(key), ".git"))
	return err == nil && s.IsDir()
}

// preReceiveHook rejects every push into the mirror. Pushes to a checked-out
// catalog branch are refused by git natively, but pushes to *other* branch
// names would land in the mirror's refs/heads/* — stray state in a namespace
// shed owns. The one realistic pusher is a half-created workspace (crash
// between clone and `remote set-url`) whose origin still points at a
// shed-owned path; this makes that push fail loudly instead of quietly
// corrupting the branch↔catalog mapping.
const preReceiveHook = `#!/bin/sh
# Installed by shed. Mirrors are fetch-only: nothing may push here.
# If you are seeing this from a workspace, its origin remote still points at
# shed's internal mirror (a crash during workspace creation); re-run
# 'shed workspace new' to repair it, or 'git remote set-url origin <upstream>'.
echo "shed: this is a shed-managed mirror; pushes are not allowed" >&2
exit 1
`

// Create makes the mirror for url at key, or returns nil if it already
// exists. Creation is `git clone --no-checkout` into a temp dir renamed into
// place — a kill -9 mid-creation must not leave a half-mirror that later
// syncs trust. Two fixups follow the clone: the local default branch git
// creates (with HEAD attached to it) sits squarely in the catalogs'
// namespace, so HEAD is detached ref-only (`update-ref --no-deref` — the
// working tree is never materialized) and the branch deleted, freeing its
// name for a catalog worktree. The forced tag refspec is added alongside the
// default one, and a pre-receive hook closes the stray-push hole.
//
// Creation-time config: extensions.worktreeConfig=true (per-catalog Git
// config without leaking to the mirror or siblings) and gc.auto=0 — not for
// safety (every catalog branch and worktree HEAD is a gc root) but for timing
// ownership: shed does the mirror's maintenance in prune, on shed's schedule.
//
// When progress is non-nil, git's live progress meter (Counting/Receiving
// objects) is streamed to it as the clone runs. Pass nil to stay quiet — the
// default for parallel syncs, where concurrent meters would interleave.
func Create(url, key string, progress io.Writer) error {
	dest := paths.MirrorPath(key)
	if Exists(key) {
		return nil
	}
	// Serialize concurrent creations of the same mirror. The mirror's own
	// lockfile lives inside the repo being created, so creation needs a
	// sibling lock: without it, two processes (a session-start bg-sync and a
	// user-run add/workspace-new are routine peers) would clone into and
	// RemoveAll the same temp path under each other. The loser blocks until
	// the winner publishes, then sees Exists and returns nil.
	lock, err := acquireCreateLock(key, createLockTimeout)
	if err != nil {
		return fmt.Errorf("mirror creation of %s: %w", key, err)
	}
	defer lock.Unlock()
	if Exists(key) {
		return nil // another process won the race and published
	}
	tmp := dest + ".tmp"
	// A leftover temp dir from a crashed prior creation is garbage; the rename
	// below is the only step that publishes a mirror. Safe under the creation
	// lock — no live clone can own this path.
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	args := []string{"clone", "--no-checkout",
		"--config", "gc.auto=0",
		"--config", "extensions.worktreeConfig=true",
	}
	if progress != nil {
		args = append(args, "--progress")
	}
	// "--" terminates options so a url beginning with "-" can't be parsed as a
	// git flag (argument injection); url and dest are strictly positional.
	args = append(args, "--", url, tmp)
	if out, err := gitx.RunProgress(progress, nil, args...); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("git clone: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	if err := finishCreate(tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		// Lost a race with another process that created the mirror first: fine,
		// the published mirror wins.
		if Exists(key) {
			return nil
		}
		return err
	}
	return nil
}

// finishCreate applies the post-clone fixups to a mirror still at its temp
// path: detach HEAD ref-only, delete the clone-created default branch, add
// the forced tag refspec, install the pre-receive hook.
func finishCreate(dir string) error {
	// The forced tag refspec is required, not optional: default tag handling
	// refuses a moved tag ("would clobber existing tag"), failing every
	// subsequent sync of that mirror forever, and --prune alone never removes
	// upstream-deleted tags.
	if err := gitx.Run(dir, "config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*"); err != nil {
		return err
	}
	// An empty upstream clones with an unborn HEAD and no local branch; both
	// fixups are then moot (and would fail), so they are gated on HEAD
	// resolving to a commit.
	if sha, ok := gitx.RevParse(dir, "HEAD"); ok {
		def, err := gitx.Output(dir, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			return fmt.Errorf("resolve clone default branch: %w", err)
		}
		// Detach without touching the (empty) working tree; a checkout here
		// would burn IO and a full tree of disk with no consumer.
		if err := gitx.Run(dir, "update-ref", "--no-deref", "HEAD", sha); err != nil {
			return err
		}
		if err := gitx.Run(dir, "branch", "-D", def); err != nil {
			return err
		}
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(hook), 0755); err != nil {
		return err
	}
	return os.WriteFile(hook, []byte(preReceiveHook), 0755)
}

// Fetch refreshes the mirror from its upstream — the only network step of a
// sync. It uses the configured refspecs (standard clone refspec + forced
// tags) with --prune --prune-tags so upstream branch/tag deletions and moves
// propagate. When progress is non-nil, git's live meter is streamed to it.
func Fetch(key string, progress io.Writer) error {
	args := []string{"-C", paths.MirrorPath(key), "fetch", "--prune", "--prune-tags"}
	if progress != nil {
		args = append(args, "--progress")
	}
	args = append(args, "origin")
	out, err := gitx.RunProgress(progress, nil, args...)
	if err != nil {
		return fmt.Errorf("git fetch: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RefreshHead re-resolves the upstream default branch (git ls-remote
// --symref) into refs/remotes/origin/HEAD, then re-points the mirror's
// detached HEAD at the default tip — ref-only, never a checkout. Best-effort
// in the small ways that don't matter (a server that hides the symref just
// keeps the clone-time origin/HEAD), but a network failure is returned so
// sync reports it.
func RefreshHead(key string) error {
	dir := paths.MirrorPath(key)
	out, err := gitx.Output(dir, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve upstream HEAD: %w", err)
	}
	if def, ok := parseSymref(out); ok {
		if err := gitx.Run(dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+def); err != nil {
			return err
		}
	}
	// Keep the detached HEAD on the default tip so it is a meaningful gc root
	// and `git -C <mirror> log` does something sensible during debugging. An
	// empty upstream has no tip; skip.
	if sha, ok := gitx.RevParse(dir, "refs/remotes/origin/HEAD"); ok {
		return gitx.Run(dir, "update-ref", "--no-deref", "HEAD", sha)
	}
	return nil
}

// parseSymref extracts the default branch short name from `git ls-remote
// --symref origin HEAD` output ("ref: refs/heads/main\tHEAD\n…").
func parseSymref(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "ref:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "refs/heads/") {
			return strings.TrimPrefix(fields[1], "refs/heads/"), true
		}
	}
	return "", false
}

// DefaultBranch returns the upstream default branch short name from the
// mirror's refs/remotes/origin/HEAD symref, without touching the network.
func DefaultBranch(key string) (string, error) {
	out, err := gitx.Output(paths.MirrorPath(key), "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("could not resolve origin/HEAD: %w", err)
	}
	return strings.TrimPrefix(out, "refs/remotes/origin/"), nil
}

// LocalBranches returns the mirror's refs/heads/* short names — by invariant,
// one per branch-tracked catalog repo of this mirror.
func LocalBranches(key string) ([]string, error) {
	out, err := gitx.Output(paths.MirrorPath(key), "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// PruneStrayBranches deletes local branches that no catalog repo of this
// mirror claims — stray state in a namespace shed owns (e.g. left behind by a
// removed repo or a crashed pre-hook push attempt). Branches checked out by a
// live worktree can't be deleted and are skipped silently; the expected set
// protects every configured catalog branch regardless. Callers hold the
// mirror's exclusive lock.
func PruneStrayBranches(key string, expected map[string]bool) {
	branches, err := LocalBranches(key)
	if err != nil {
		return
	}
	dir := paths.MirrorPath(key)
	for _, b := range branches {
		if !expected[b] {
			_ = gitx.Run(dir, "branch", "-D", b)
		}
	}
}

// Gc runs the mirror's garbage collection. Callers must hold the mirror's
// exclusive lock: gc is safe for correctness at any moment (catalog branches,
// tag refs, remote-tracking refs, and worktree HEADs are all reachability
// roots), but scheduling it under the lock keeps workspace clones from racing
// a repack.
func Gc(key string) error {
	return gitx.Run(paths.MirrorPath(key), "gc")
}

// PruneWorktrees drops stale worktree bookkeeping (admin dirs whose checkout
// vanished) from the mirror.
func PruneWorktrees(key string) error {
	return gitx.Run(paths.MirrorPath(key), "worktree", "prune")
}

// Remove deletes the mirror from disk under its exclusive lock. Only prune
// calls this, and only for mirrors no config entry references.
func Remove(key string, timeout time.Duration) error {
	if !Exists(key) {
		return nil
	}
	lock, err := AcquireLock(key, true, timeout)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	p := paths.MirrorPath(key)
	if err := os.RemoveAll(p); err != nil {
		return err
	}
	// Drop the sibling creation lockfile too, before pruning empty parents
	// (a stray file would keep the parent dir alive).
	_ = os.Remove(paths.MirrorCreateLockFile(key))
	paths.PruneEmptyDirs(filepath.Dir(p), paths.MirrorsDir())
	return nil
}

// OnDisk returns the keys of every mirror present under MirrorsDir, for
// prune's orphan sweep.
func OnDisk() ([]string, error) {
	root := paths.MirrorsDir()
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var keys []string
	walkErr := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		// A "<key>.tmp" dir is an in-progress (or crashed) creation, not a
		// mirror: reporting it would let prune's orphan sweep delete a clone
		// out from under the process building it. Crashed leftovers are
		// reclaimed by the next Create of that key, under the creation lock.
		if strings.HasSuffix(info.Name(), ".tmp") {
			return filepath.SkipDir
		}
		if s, err := os.Stat(filepath.Join(p, ".git")); err == nil && s.IsDir() {
			rel, err := filepath.Rel(root, p)
			if err != nil || rel == "." {
				return nil
			}
			keys = append(keys, filepath.ToSlash(rel))
			return filepath.SkipDir
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return keys, nil
}

// Size returns the mirror's total on-disk size in bytes — where a repo's real
// bytes live under this model (catalog worktrees carry only a checkout).
// Errors on individual files are ignored; a missing mirror is 0.
func Size(key string) (int64, error) {
	if !Exists(key) {
		return 0, nil
	}
	var total int64
	err := filepath.Walk(paths.MirrorPath(key), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// Lock is a held flock on the mirror's lockfile.
type Lock struct{ inner *flock.Flock }

// AcquireLock takes the mirror's flock: exclusive for sync phases, worktree
// operations, and gc; shared for workspace creation (many clones may read
// concurrently, but never during a repack). Callers needing both modes use
// two separate acquisitions, never an in-place upgrade (deadlocks). Returns
// ErrLocked on timeout, and an error when the mirror is not on disk — the
// lockfile lives in the mirror's .git, and creating directories here would
// fabricate the very marker Exists() trusts, turning a locked-but-absent
// mirror into a permanently poisoned half-mirror.
func AcquireLock(key string, exclusive bool, timeout time.Duration) (*Lock, error) {
	p := paths.MirrorLockFile(key)
	if fi, err := os.Stat(filepath.Dir(p)); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("mirror %s is not on disk (removed concurrently?)", key)
	}
	return flockWithTimeout(p, exclusive, timeout)
}

// createLockTimeout bounds how long a creation loser waits for the winner's
// clone. First clones of large repos legitimately take many minutes, and the
// loser needs the mirror anyway, so waiting is the correct behavior.
const createLockTimeout = 30 * time.Minute

// acquireCreateLock takes the mirror's creation lock — a sibling file, since
// the mirror (and its in-repo lockfile) doesn't exist yet.
func acquireCreateLock(key string, timeout time.Duration) (*Lock, error) {
	p := paths.MirrorCreateLockFile(key)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return nil, err
	}
	return flockWithTimeout(p, true, timeout)
}

// flockWithTimeout polls for the flock at p until timeout. gofrs/flock's
// TryLockContext reports a deadline as (false, ctx.Err()), so the timeout is
// translated to ErrLocked here — callers classify lock contention with
// errors.Is(err, ErrLocked).
func flockWithTimeout(p string, exclusive bool, timeout time.Duration) (*Lock, error) {
	l := flock.New(p)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var (
		ok  bool
		err error
	)
	if exclusive {
		ok, err = l.TryLockContext(ctx, 100*time.Millisecond)
	} else {
		ok, err = l.TryRLockContext(ctx, 100*time.Millisecond)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrLocked
		}
		return nil, err
	}
	if !ok {
		return nil, ErrLocked
	}
	return &Lock{inner: l}, nil
}

func (l *Lock) Unlock() error { return l.inner.Unlock() }
