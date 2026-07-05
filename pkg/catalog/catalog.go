// Package catalog manages the read-only catalog repos under ~/.shed/repos:
// permanent, browsable checkouts, each a worktree of its upstream's mirror.
// A branch-tracked catalog sits on a real local branch (git status says
// "On branch main") that sync fast-forwards; a tag-tracked catalog is a named
// detached checkout that never changes unless the tag itself moves.
//
// A catalog repo owns almost no state: its branch is the state, its identity
// lives in config, and its sync record lives on the mirror's meta. Its .git
// is a pointer file into the mirror — which is why validity means "the .git
// pointer resolves", not "the directory exists", and why a broken catalog is
// repaired by re-adding the worktree rather than re-cloning anything.
//
// Only shed writes to catalogs (ff-merges on sync); agents only ever write to
// workspaces. The working tree is kept chmod a-w between syncs to make that
// hard to violate by accident.
package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// ErrEmptyUpstream marks an upstream with no commits at all: there is nothing
// to check out, so the repo holds an "empty" state with no directory and
// materializes on the first sync after upstream gains commits.
var ErrEmptyUpstream = errors.New("upstream has no commits yet")

// lfsSkip keeps every catalog checkout from invoking the LFS smudge filter: a
// read-only catalog only needs the committed pointer files, not the resolved
// blobs. Without it, checkouts would fetch every LFS object on each sync, and
// a single missing object would fail the whole sync.
var lfsSkip = []string{"GIT_LFS_SKIP_SMUDGE=1"}

// Ref is a resolved track: the short name of the branch or tag a catalog
// checks out.
type Ref struct {
	Short string
	IsTag bool
}

// Path returns the catalog repo's on-disk path. Does not check existence.
func Path(name string) string { return paths.CatalogPath(name) }

// Exists reports whether the catalog directory is present on disk. Presence
// is deliberately weaker than validity — see Valid.
func Exists(name string) bool {
	s, err := os.Stat(paths.CatalogPath(name))
	return err == nil && s.IsDir()
}

// Valid reports whether the catalog is a live worktree: its .git pointer
// resolves. A zombie left by an interrupted removal (read-only dir, dangling
// .git pointer, invisible to `git worktree list`) exists but is not valid;
// sync repairs it by re-adding the worktree.
func Valid(name string) bool {
	if !Exists(name) {
		return false
	}
	_, err := gitx.Output(paths.CatalogPath(name), "rev-parse", "--git-dir")
	return err == nil
}

// ResolveTrack resolves a config track value against the mirror's fetched
// refs. An empty track means the upstream default branch. Bare short names
// prefer a branch over a tag (matching `git clone --branch`); the full-ref
// forms heads/<n> and tags/<n> pin the kind. Returns ErrEmptyUpstream when
// the mirror has no refs at all, and a plain-language "no longer exists
// upstream" error for a missing ref — sync runs this pre-check so a deleted
// branch never surfaces as a git internals error.
func ResolveTrack(mirrorKey, track string) (Ref, error) {
	dir := paths.MirrorPath(mirrorKey)
	if empty, err := noRefs(dir); err != nil {
		return Ref{}, err
	} else if empty {
		return Ref{}, ErrEmptyUpstream
	}
	if track == "" {
		out, err := gitx.Output(dir, "symbolic-ref", "refs/remotes/origin/HEAD")
		if err != nil {
			return Ref{}, fmt.Errorf("could not resolve the upstream default branch: %w", err)
		}
		short := strings.TrimPrefix(out, "refs/remotes/origin/")
		if ok, _ := gitx.RefExists(dir, "refs/remotes/origin/"+short); !ok {
			return Ref{}, fmt.Errorf("upstream default branch %q has no fetched ref", short)
		}
		return Ref{Short: short}, nil
	}
	short, kind := paths.ParseTrack(track)
	isBranch, _ := gitx.RefExists(dir, "refs/remotes/origin/"+short)
	isTag, _ := gitx.RefExists(dir, "refs/tags/"+short)
	switch kind {
	case paths.TrackBranch:
		if isBranch {
			return Ref{Short: short}, nil
		}
	case paths.TrackTag:
		if isTag {
			return Ref{Short: short, IsTag: true}, nil
		}
	default:
		if isBranch {
			return Ref{Short: short}, nil
		}
		if isTag {
			return Ref{Short: short, IsTag: true}, nil
		}
	}
	return Ref{}, fmt.Errorf("track %q no longer exists upstream (no such branch or tag)", track)
}

// noRefs reports whether the mirror has no fetched refs at all (an upstream
// with no commits).
func noRefs(dir string) (bool, error) {
	out, err := gitx.Output(dir, "for-each-ref", "--count=1",
		"refs/remotes/origin", "refs/tags")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// Ensure brings the catalog to its synced state: create it if missing,
// repair it if broken, fast-forward it if behind. Callers hold the mirror's
// exclusive lock. The returned note describes what happened for sync output:
// "" (already current), "created", "updated", "reset (force-pushed
// upstream)", or "tag moved".
//
// The repair pass covers the routine damage sync must absorb: a worktree an
// agent switched off its designated branch (re-checkout), a stale index.lock
// from a kill -9 mid-update (remove — git never cleans it), and a dangling
// .git pointer from an interrupted removal (remove and re-add). Repair never
// consults the network; everything here is local, deterministic, and
// retryable.
func Ensure(mirrorKey, name string, ref Ref, gitConfig map[string]string) (note string, err error) {
	path := paths.CatalogPath(name)

	// A directory that is not a live worktree is a zombie: make it writable,
	// remove it, drop its stale bookkeeping, and fall through to creation.
	if Exists(name) && !Valid(name) {
		_ = UnlockTree(name)
		if err := os.RemoveAll(path); err != nil {
			return "", fmt.Errorf("remove broken catalog: %w", err)
		}
		_ = gitx.Run(paths.MirrorPath(mirrorKey), "worktree", "prune")
	}

	created := false
	if !Exists(name) {
		if err := create(mirrorKey, name, ref); err != nil {
			return "", err
		}
		created = true
	}

	unlocked := created // create leaves the tree writable until the final lock
	if !created {
		// A writable root is the fingerprint of an interrupted prior pass
		// (killed between UnlockTree and LockTree): LockTree re-locks the
		// root only after every child, so a fully locked tree always has a
		// read-only root. Treat it as unlocked so the final LockTree below
		// re-establishes the invariant instead of skipping forever.
		if treeWritable(name) {
			unlocked = true
		}
		u, err := repair(name, ref)
		if err != nil {
			return "", err
		}
		unlocked = unlocked || u
	}

	// Per-repo git config is reconciled on every pass (cheap — no tree walk).
	// --worktree scopes it to this catalog: extensions.worktreeConfig is set
	// at mirror creation, so values never leak to the mirror or siblings.
	if err := applyConfig(path, gitConfig); err != nil {
		return "", err
	}

	updated, updateNote, err := update(name, ref, unlocked)
	if err != nil {
		return "", err
	}
	if created || updated || unlocked {
		if err := LockTree(name); err != nil {
			return "", fmt.Errorf("chmod a-w: %w", err)
		}
	}
	switch {
	case created:
		return "created", nil
	case updateNote != "":
		return updateNote, nil
	case updated:
		return "updated", nil
	default:
		return "", nil
	}
}

// create adds the catalog worktree from the mirror: on a real local branch
// for a branch track (git status reads "On branch <track>"; git refuses a
// second checkout of the same branch, the belt-and-suspenders behind the one
// repo per (url, track) invariant), or as a named detached checkout for a tag
// (git cannot be "on" a tag; the named detach makes status read "HEAD
// detached at <tag>").
func create(mirrorKey, name string, ref Ref) error {
	mirrorDir := paths.MirrorPath(mirrorKey)
	path := paths.CatalogPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	// Stale bookkeeping for this path (a catalog dir deleted without shed's
	// help) would fail the add; prune is cheap and idempotent.
	_ = gitx.Run(mirrorDir, "worktree", "prune")

	if ref.IsTag {
		target := ref.Short
		// A bare short name resolves branch-first in git's rev machinery; if a
		// local or remote branch shares the tag's name, pin it explicitly.
		if b, _ := gitx.RefExists(mirrorDir, "refs/heads/"+ref.Short); b {
			target = "tags/" + ref.Short
		} else if rb, _ := gitx.RefExists(mirrorDir, "refs/remotes/origin/"+ref.Short); rb {
			target = "tags/" + ref.Short
		}
		if err := gitx.RunEnv(mirrorDir, lfsSkip, "worktree", "add", "--detach", path, target); err != nil {
			return err
		}
		// Purely cosmetic (it only affects what `git status` prints), so a
		// failure — e.g. a reftable-backend repo with no logs/HEAD file —
		// must not fail catalog creation and leave the tree half-made.
		_ = nameDetach(path, ref.Short)
		return nil
	}
	// The local branch normally doesn't exist yet (it is created here, pinned
	// to the upstream ref); after some repair histories it may already exist,
	// in which case checking it out directly is the right move.
	if exists, _ := gitx.RefExists(mirrorDir, "refs/heads/"+ref.Short); exists {
		return gitx.RunEnv(mirrorDir, lfsSkip, "worktree", "add", path, ref.Short)
	}
	return gitx.RunEnv(mirrorDir, lfsSkip, "worktree", "add", "--track",
		"-b", ref.Short, path, "origin/"+ref.Short)
}

// nameDetach appends the HEAD reflog entry a real `git checkout <tag>` would
// have written. `git worktree add --detach` logs an entry with an empty
// message, and a same-commit checkout appends nothing — either way `git
// status` has no "checkout: moving from … to <name>" line to parse, so it
// reads "Not currently on any branch" instead of naming the tag. Appending
// the entry ourselves (under the mirror's exclusive lock) makes status read
// "HEAD detached at <name>" — the named detach the tag tier promises.
func nameDetach(path, name string) error {
	// The append below assumes the files reflog backend; on a reftable-backend
	// repo (git >= 2.45 opt-in) a hand-written logs/HEAD would be ignored —
	// skip rather than plant a stray file.
	if backend, err := gitx.Output(path, "config", "extensions.refstorage"); err == nil && backend == "reftable" {
		return nil
	}
	sha, ok := gitx.RevParse(path, "HEAD")
	if !ok {
		return fmt.Errorf("name detach: HEAD does not resolve in %s", path)
	}
	logPath, err := gitx.Output(path, "rev-parse", "--git-path", "logs/HEAD")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(logPath) {
		logPath = filepath.Join(path, logPath)
	}
	line := fmt.Sprintf("%s %s shed <shed@localhost> %d +0000\tcheckout: moving from %s to %s\n",
		sha, sha, time.Now().Unix(), sha, name)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// repair fixes agent-inflicted or crash-inflicted damage on an existing
// catalog. Returns whether it had to unlock the working tree.
func repair(name string, ref Ref) (unlocked bool, err error) {
	path := paths.CatalogPath(name)

	// A kill -9 mid-update routinely leaves index.lock behind, and git never
	// cleans it up; every later git write would fail "unable to create
	// index.lock". We hold the exclusive lock, so nothing legitimate owns it.
	if gitDir, gerr := gitx.Output(path, "rev-parse", "--absolute-git-dir"); gerr == nil {
		_ = os.Remove(filepath.Join(gitDir, "index.lock"))
	}

	if !ref.IsTag {
		// Detached or on the wrong branch (an agent ran checkout inside the
		// read-only catalog): put it back on its designated branch.
		cur, cerr := gitx.Output(path, "symbolic-ref", "--short", "HEAD")
		if cerr != nil || cur != ref.Short {
			if err := UnlockTree(name); err != nil {
				return false, fmt.Errorf("chmod u+w: %w", err)
			}
			unlocked = true
			if err := gitx.RunEnv(path, lfsSkip, "checkout", "--force", ref.Short); err != nil {
				// The branch itself may be gone (deleted from inside the
				// worktree); recreate it at the upstream ref.
				if err2 := gitx.RunEnv(path, lfsSkip, "checkout", "--force", "-B", ref.Short,
					"refs/remotes/origin/"+ref.Short); err2 != nil {
					return unlocked, err
				}
			}
		}
	}

	// Tracked files modified or deleted in place (an agent that chmod'd the
	// tree writable) leave HEAD untouched, so the skip-if-current fast path
	// would preserve the damage forever. One status probe per sync keeps the
	// catalog self-healing; untracked files are ignored — they don't corrupt
	// tracked content, and the old model's forced checkout left them alone
	// too.
	dirty, derr := gitx.Output(path, "status", "--porcelain", "--untracked-files=no")
	if derr == nil && dirty != "" {
		if !unlocked {
			if err := UnlockTree(name); err != nil {
				return false, fmt.Errorf("chmod u+w: %w", err)
			}
			unlocked = true
		}
		if err := gitx.RunEnv(path, lfsSkip, "reset", "--hard", "HEAD"); err != nil {
			return unlocked, err
		}
	}
	return unlocked, nil
}

// update advances the catalog to its upstream ref if it moved. The common
// case — already current — is detected without touching the tree, so a sync
// that changes nothing costs no chmod walks.
func update(name string, ref Ref, alreadyUnlocked bool) (updated bool, note string, err error) {
	path := paths.CatalogPath(name)
	upstream := "refs/remotes/origin/" + ref.Short
	if ref.IsTag {
		upstream = "refs/tags/" + ref.Short + "^{commit}"
	}
	want, ok := gitx.RevParse(path, upstream)
	if !ok {
		return false, "", fmt.Errorf("track %q no longer exists upstream", ref.Short)
	}
	head, _ := gitx.RevParse(path, "HEAD")
	if head == want {
		return false, "", nil
	}
	if !alreadyUnlocked {
		if err := UnlockTree(name); err != nil {
			return false, "", fmt.Errorf("chmod u+w: %w", err)
		}
	}
	if ref.IsTag {
		// The tag itself moved (forced tag refspec propagates moves); re-detach
		// at its new target, by name so status stays a named detach.
		if err := gitx.RunEnv(path, lfsSkip, "checkout", "--force", "--detach", "tags/"+ref.Short); err != nil {
			return true, "", err
		}
		return true, "tag moved", nil
	}
	if err := gitx.RunEnv(path, lfsSkip, "merge", "--ff-only", upstream); err != nil {
		// A force-pushed upstream can't fast-forward; the catalog carries no
		// local work by invariant, so a hard reset to upstream is the repair —
		// reported in plain language, not as a git error.
		if err2 := gitx.RunEnv(path, lfsSkip, "reset", "--hard", upstream); err2 != nil {
			return true, "", err2
		}
		return true, "reset (force-pushed upstream)", nil
	}
	return true, "", nil
}

// applyConfig writes per-repo git config into the catalog's worktree-scoped
// config. Set/update only — a key removed from config is not unset here.
// Keys are applied in sorted order for deterministic behavior; callers
// validate them up front (config.ValidateGitConfigKey).
func applyConfig(path string, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := gitx.Run(path, "config", "--worktree", k, kv[k]); err != nil {
			return fmt.Errorf("git config %s: %w", k, err)
		}
	}
	return nil
}

// Remove deletes the catalog: unlock the tree FIRST, then `git worktree
// remove` — in the other order, remove deregisters the worktree and then
// fails the deletion, leaving a zombie. Callers hold the mirror's exclusive
// lock when the mirror exists. Zombies and catalogs whose mirror is already
// gone fall back to plain removal. Returns nil if already absent.
func Remove(mirrorKey, name string) error {
	path := paths.CatalogPath(name)
	if !Exists(name) {
		return nil
	}
	_ = UnlockTree(name)
	mirrorDir := paths.MirrorPath(mirrorKey)
	if Valid(name) {
		if err := gitx.Run(mirrorDir, "worktree", "remove", "--force", path); err == nil {
			paths.PruneEmptyDirs(filepath.Dir(path), paths.ReposDir())
			return nil
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	_ = gitx.Run(mirrorDir, "worktree", "prune")
	paths.PruneEmptyDirs(filepath.Dir(path), paths.ReposDir())
	return nil
}

// LockTree applies chmod -R a-w to the catalog working tree, excluding its
// .git pointer. The owner can always re-chmod to restore write later.
func LockTree(name string) error { return chmodTree(paths.CatalogPath(name), false) }

// UnlockTree applies chmod -R u+w to the catalog working tree, excluding its
// .git pointer, so a subsequent git checkout/merge can write tracked files.
func UnlockTree(name string) error { return chmodTree(paths.CatalogPath(name), true) }

// treeWritable reports whether the catalog root directory carries the owner
// write bit. LockTree removes write from the root only after every child, and
// UnlockTree restores it on the root first — so a writable root is a reliable
// fingerprint of a tree that is (or may be) not fully locked, whatever
// interruption produced it.
func treeWritable(name string) bool {
	fi, err := os.Stat(paths.CatalogPath(name))
	return err == nil && fi.Mode().Perm()&0200 != 0
}

func chmodTree(root string, writable bool) error {
	gitPath := filepath.Join(root, ".git")
	gitPathPrefix := gitPath + string(filepath.Separator)
	chmod := func(p string, info os.FileInfo) error {
		mode := info.Mode().Perm()
		if writable {
			mode |= 0200
		} else {
			mode &^= 0222
		}
		if mode == info.Mode().Perm() {
			return nil
		}
		return os.Chmod(p, mode)
	}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			// When locking, the root is deliberately chmod'd LAST (below), so
			// a read-only root always means "every child was locked" — the
			// invariant treeWritable relies on. When unlocking, the root goes
			// first anyway (Walk is pre-order) so children can be traversed.
			if !writable {
				return nil
			}
			return chmod(p, info)
		}
		if p == gitPath {
			// A catalog's .git is a pointer FILE into the mirror. SkipDir from
			// a non-dir entry would skip the rest of the parent dir — i.e.
			// silently leave most of the repo root writable — so only a real
			// .git directory (a workspace-style layout) gets SkipDir.
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(p, gitPathPrefix) {
			return nil
		}
		return chmod(p, info)
	})
	if err != nil || writable {
		return err
	}
	fi, err := os.Stat(root)
	if err != nil {
		return err
	}
	return chmod(root, fi)
}

// OnDisk returns the names of every catalog-shaped directory under ReposDir
// (a dir containing a .git entry — file or dir), for sync's orphan detection:
// changing `track` is an identity change, so a stale dir with no config entry
// should be noticed and offered for pruning rather than silently kept.
func OnDisk() ([]string, error) {
	root := paths.ReposDir()
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	walkErr := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			rel, err := filepath.Rel(root, p)
			if err != nil || rel == "." {
				return nil
			}
			names = append(names, filepath.ToSlash(rel))
			return filepath.SkipDir
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return names, nil
}
