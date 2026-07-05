package catalog

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

const key = "github.com/acme/widget"

// tempHome points HOME at a fresh temp dir and registers a cleanup that
// restores the owner write bit on every directory beneath it, so t.TempDir
// removal can delete the read-only trees Ensure leaves behind (chmod a-w,
// see LockTree — unlinking an entry needs write on its parent directory).
func tempHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() {
		filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() && info.Mode().Perm()&0200 == 0 {
				os.Chmod(p, info.Mode().Perm()|0200)
			}
			return nil
		})
	})
}

// setup creates an upstream (main + branch "rel" + tag "v1") and its mirror,
// returning the upstream path.
func setup(t *testing.T) string {
	t.Helper()
	requireGit(t)
	tempHome(t)
	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	git(t, root, "init", "-q", "-b", "main", up)
	writeUp(t, up, "a.txt", "1")
	git(t, up, "add", "a.txt")
	git(t, up, "commit", "-q", "-m", "first")
	git(t, up, "tag", "v1")
	git(t, up, "branch", "rel")
	if err := mirror.Create(up, key, nil); err != nil {
		t.Fatalf("mirror.Create: %v", err)
	}
	return up
}

func writeUp(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// commitUpstream adds a commit on the given upstream branch and refreshes the
// mirror.
func commitUpstream(t *testing.T, up, branch, file string) {
	t.Helper()
	git(t, up, "checkout", "-q", branch)
	writeUp(t, up, file, file)
	git(t, up, "add", file)
	git(t, up, "commit", "-q", "-m", file)
	if err := mirror.Fetch(key, nil); err != nil {
		t.Fatalf("mirror.Fetch: %v", err)
	}
}

func TestResolveTrack(t *testing.T) {
	setup(t)
	cases := []struct {
		track   string
		want    Ref
		wantErr bool
	}{
		{"", Ref{Short: "main"}, false},              // default branch
		{"rel", Ref{Short: "rel"}, false},            // branch by short name
		{"v1", Ref{Short: "v1", IsTag: true}, false}, // tag by short name
		{"heads/rel", Ref{Short: "rel"}, false},      // pinned branch
		{"tags/v1", Ref{Short: "v1", IsTag: true}, false},
		{"heads/v1", Ref{}, true}, // pinned to a branch that doesn't exist
		{"nope", Ref{}, true},
	}
	for _, tc := range cases {
		got, err := ResolveTrack(key, tc.track)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ResolveTrack(%q): want error, got %+v", tc.track, got)
			} else if !strings.Contains(err.Error(), "upstream") {
				t.Errorf("ResolveTrack(%q): error should read plainly, got %v", tc.track, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ResolveTrack(%q) = %+v, %v; want %+v", tc.track, got, err, tc.want)
		}
	}
}

// A bare short name that names BOTH a branch and a tag resolves to the
// branch, matching `git clone --branch`.
func TestResolveTrackPrefersBranch(t *testing.T) {
	up := setup(t)
	git(t, up, "tag", "rel") // now "rel" is a branch AND a tag
	if err := mirror.Fetch(key, nil); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveTrack(key, "rel")
	if err != nil || got.IsTag {
		t.Fatalf("bare name should prefer the branch, got %+v (err=%v)", got, err)
	}
	got, err = ResolveTrack(key, "tags/rel")
	if err != nil || !got.IsTag {
		t.Fatalf("tags/ form should pin the tag, got %+v (err=%v)", got, err)
	}
}

// The full lifecycle of a branch catalog: create (on a real branch, tree
// read-only), skip-if-current, fast-forward on upstream movement, hard reset
// on force-push, and repair after agent mischief.
func TestEnsureBranchCatalogLifecycle(t *testing.T) {
	up := setup(t)
	const name = key // default-branch catalog

	note, err := Ensure(key, name, Ref{Short: "main"}, nil)
	if err != nil || note != "created" {
		t.Fatalf("first Ensure = %q, %v; want created", note, err)
	}
	dir := paths.CatalogPath(name)
	// On a real branch.
	if cur, _ := gitx.Output(dir, "symbolic-ref", "--short", "HEAD"); cur != "main" {
		t.Errorf("catalog should be on branch main, got %q", cur)
	}
	// Tree content present and read-only.
	fi, err := os.Stat(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("checked-out file missing: %v", err)
	}
	if fi.Mode().Perm()&0222 != 0 {
		t.Errorf("tree should be read-only, a.txt mode %v", fi.Mode())
	}
	// Second Ensure with nothing new: the skip path.
	note, err = Ensure(key, name, Ref{Short: "main"}, nil)
	if err != nil || note != "" {
		t.Fatalf("current Ensure = %q, %v; want no-op", note, err)
	}

	// Upstream advances → ff.
	commitUpstream(t, up, "main", "b.txt")
	note, err = Ensure(key, name, Ref{Short: "main"}, nil)
	if err != nil {
		t.Fatalf("Ensure after upstream commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
		t.Errorf("ff should have materialized b.txt: %v", err)
	}
	head, _ := gitx.RevParse(dir, "HEAD")
	want, _ := gitx.RevParse(dir, "refs/remotes/origin/main")
	if head != want {
		t.Errorf("catalog HEAD %s, want origin/main %s", head, want)
	}

	// Upstream force-push → reset, reported in plain language.
	git(t, up, "reset", "-q", "--hard", "HEAD~1")
	writeUp(t, up, "c.txt", "3")
	git(t, up, "add", "c.txt")
	git(t, up, "commit", "-q", "-m", "rewritten")
	if err := mirror.Fetch(key, nil); err != nil {
		t.Fatal(err)
	}
	note, err = Ensure(key, name, Ref{Short: "main"}, nil)
	if err != nil {
		t.Fatalf("Ensure after force-push: %v", err)
	}
	if !strings.Contains(note, "force-pushed") {
		t.Errorf("force-push should be reported, got note %q", note)
	}
	head, _ = gitx.RevParse(dir, "HEAD")
	want, _ = gitx.RevParse(dir, "refs/remotes/origin/main")
	if head != want {
		t.Errorf("after reset catalog HEAD %s, want %s", head, want)
	}
}

// Agent mischief inside a catalog: switching it off its branch, and leaving a
// stale index.lock. Both are repaired by the next Ensure, and neither can
// block the mirror fetch (fetch only writes refs/remotes/*).
func TestEnsureRepairsAgentMischief(t *testing.T) {
	up := setup(t)
	const name = key
	if _, err := Ensure(key, name, Ref{Short: "main"}, nil); err != nil {
		t.Fatal(err)
	}
	dir := paths.CatalogPath(name)

	// Detach the catalog and drop a stale index.lock, as a crashed or rogue
	// agent might.
	git(t, dir, "checkout", "-q", "--detach", "HEAD")
	gitDir, err := gitx.Output(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "index.lock"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	// The fetch is structurally unblockable regardless of catalog state.
	commitUpstream(t, up, "main", "d.txt")

	if _, err := Ensure(key, name, Ref{Short: "main"}, nil); err != nil {
		t.Fatalf("Ensure should repair mischief: %v", err)
	}
	if cur, _ := gitx.Output(dir, "symbolic-ref", "--short", "HEAD"); cur != "main" {
		t.Errorf("repair should re-checkout main, got %q", cur)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "index.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale index.lock should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d.txt")); err != nil {
		t.Errorf("repaired catalog should be at origin/main: %v", err)
	}
}

// A zombie catalog (directory present, .git pointer dangling — the leftover
// of an interrupted removal) is invalid and gets rebuilt.
func TestEnsureRebuildsZombie(t *testing.T) {
	setup(t)
	const name = key
	if _, err := Ensure(key, name, Ref{Short: "main"}, nil); err != nil {
		t.Fatal(err)
	}
	dir := paths.CatalogPath(name)

	// Fake the zombie: deregister the worktree from the mirror but leave the
	// directory (what `worktree remove` against a locked tree produces).
	if err := os.RemoveAll(filepath.Join(paths.MirrorPath(key), ".git", "worktrees")); err != nil {
		t.Fatal(err)
	}
	if Valid(name) {
		t.Fatal("dangling .git pointer should be invalid")
	}
	if !Exists(name) {
		t.Fatal("zombie should still exist on disk")
	}

	note, err := Ensure(key, name, Ref{Short: "main"}, nil)
	if err != nil || note != "created" {
		t.Fatalf("Ensure should rebuild the zombie: %q, %v", note, err)
	}
	if !Valid(name) {
		t.Error("rebuilt catalog should be valid")
	}
	if fi, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil || fi.Mode().Perm()&0222 != 0 {
		t.Errorf("rebuilt tree should be checked out read-only (err=%v)", err)
	}
}

// A tag catalog is a named detached checkout that only moves when the tag
// itself does.
func TestEnsureTagCatalog(t *testing.T) {
	up := setup(t)
	name := key + "@v1"

	note, err := Ensure(key, name, Ref{Short: "v1", IsTag: true}, nil)
	if err != nil || note != "created" {
		t.Fatalf("Ensure = %q, %v; want created", note, err)
	}
	dir := paths.CatalogPath(name)
	if _, err := gitx.Output(dir, "symbolic-ref", "HEAD"); err == nil {
		t.Error("tag catalog should be detached")
	}
	tagSha, _ := gitx.RevParse(dir, "refs/tags/v1^{commit}")
	head, _ := gitx.RevParse(dir, "HEAD")
	if head != tagSha {
		t.Errorf("HEAD %s, want tag commit %s", head, tagSha)
	}

	// Upstream commits on main don't touch a tag catalog.
	commitUpstream(t, up, "main", "b.txt")
	note, err = Ensure(key, name, Ref{Short: "v1", IsTag: true}, nil)
	if err != nil || note != "" {
		t.Fatalf("unmoved tag should be a no-op, got %q, %v", note, err)
	}

	// The tag itself moves → the catalog follows and says so.
	git(t, up, "tag", "-f", "v1")
	if err := mirror.Fetch(key, nil); err != nil {
		t.Fatal(err)
	}
	note, err = Ensure(key, name, Ref{Short: "v1", IsTag: true}, nil)
	if err != nil || note != "tag moved" {
		t.Fatalf("moved tag Ensure = %q, %v; want tag moved", note, err)
	}
	head, _ = gitx.RevParse(dir, "HEAD")
	want, _ := gitx.RevParse(dir, "refs/tags/v1^{commit}")
	if head != want {
		t.Errorf("catalog should follow the moved tag: HEAD %s want %s", head, want)
	}
}

// Two catalogs of one upstream coexist as branch worktrees of the same
// mirror, and removal of one leaves the other (and the mirror) intact.
func TestSideBySideCatalogsAndRemove(t *testing.T) {
	setup(t)
	main := key
	rel := key + "@rel"
	if _, err := Ensure(key, main, Ref{Short: "main"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(key, rel, Ref{Short: "rel"}, nil); err != nil {
		t.Fatal(err)
	}
	if cur, _ := gitx.Output(paths.CatalogPath(rel), "symbolic-ref", "--short", "HEAD"); cur != "rel" {
		t.Errorf("second catalog should sit on branch rel, got %q", cur)
	}

	if err := Remove(key, rel); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if Exists(rel) {
		t.Error("removed catalog dir should be gone")
	}
	if !Valid(main) {
		t.Error("sibling catalog must survive removal")
	}
	// The freed branch can be swept as stray now.
	mirror.PruneStrayBranches(key, map[string]bool{"main": true})
	branches, _ := mirror.LocalBranches(key)
	if len(branches) != 1 || branches[0] != "main" {
		t.Errorf("want only main to remain, got %v", branches)
	}
}

// Per-repo git config lands in the catalog's worktree-scoped config, without
// leaking to the mirror (and thus to sibling catalogs).
func TestEnsureAppliesWorktreeConfig(t *testing.T) {
	setup(t)
	const name = key
	if _, err := Ensure(key, name, Ref{Short: "main"}, map[string]string{"user.email": "me@work.com"}); err != nil {
		t.Fatal(err)
	}
	got, err := gitx.Output(paths.CatalogPath(name), "config", "user.email")
	if err != nil || got != "me@work.com" {
		t.Errorf("worktree config not applied: %q, %v", got, err)
	}
	if out, err := gitx.Output(paths.MirrorPath(key), "config", "user.email"); err == nil && out == "me@work.com" {
		t.Error("per-repo config leaked to the mirror")
	}
}

// An upstream with no commits is the "empty" state, not an error.
func TestResolveTrackEmptyUpstream(t *testing.T) {
	requireGit(t)
	tempHome(t)
	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	git(t, root, "init", "-q", "-b", "main", up)
	if err := mirror.Create(up, key, nil); err != nil {
		t.Fatalf("mirror.Create on empty upstream: %v", err)
	}
	if _, err := ResolveTrack(key, ""); !errors.Is(err, ErrEmptyUpstream) {
		t.Fatalf("want ErrEmptyUpstream, got %v", err)
	}
}

// The chmod walk must not stop at the .git pointer FILE: everything in the
// tree, including entries sorted after ".git", gets locked.
func TestLockTreeCoversWholeTree(t *testing.T) {
	setup(t)
	const name = key
	if _, err := Ensure(key, name, Ref{Short: "main"}, nil); err != nil {
		t.Fatal(err)
	}
	dir := paths.CatalogPath(name)
	if err := UnlockTree(name); err != nil {
		t.Fatal(err)
	}
	// "zzz.txt" sorts after ".git"; with the old SkipDir-on-file bug it would
	// silently stay writable.
	if err := os.WriteFile(filepath.Join(dir, "zzz.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := LockTree(name); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "zzz.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0222 != 0 {
		t.Errorf("file sorted after .git should be locked, mode %v", fi.Mode())
	}
}

// The tag catalog's detach is NAMED: `git status` reads "HEAD detached at
// <tag>", not "Not currently on any branch" (worktree add alone writes no
// HEAD reflog entry to name it from).
func TestTagCatalogNamedDetach(t *testing.T) {
	setup(t)
	name := key + "@v1"
	if _, err := Ensure(key, name, Ref{Short: "v1", IsTag: true}, nil); err != nil {
		t.Fatal(err)
	}
	out, err := gitx.Output(paths.CatalogPath(name), "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "HEAD detached at v1") {
		t.Errorf("status should name the tag detach, got:\n%s", out)
	}
}

// A catalog left writable by an interrupted prior pass (killed between
// UnlockTree and LockTree) must be re-locked by the next Ensure, even though
// nothing else changed — the skip-if-current fast path must not preserve a
// writable "read-only" tree forever.
func TestEnsureRelocksInterruptedTree(t *testing.T) {
	setup(t)
	const name = key
	if _, err := Ensure(key, name, Ref{Short: "main"}, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash window: tree unlocked, nothing else wrong.
	if err := UnlockTree(name); err != nil {
		t.Fatal(err)
	}
	if !treeWritable(name) {
		t.Fatal("test setup: tree should read as writable after UnlockTree")
	}

	if _, err := Ensure(key, name, Ref{Short: "main"}, nil); err != nil {
		t.Fatalf("Ensure on an unlocked-but-current catalog: %v", err)
	}
	if treeWritable(name) {
		t.Error("Ensure should have re-locked the interrupted tree")
	}
	fi, err := os.Stat(filepath.Join(paths.CatalogPath(name), "a.txt"))
	if err != nil || fi.Mode().Perm()&0222 != 0 {
		t.Errorf("files should be read-only again, mode=%v err=%v", fi.Mode(), err)
	}
}

// Tracked files modified or deleted in place (HEAD untouched) are healed by
// the next Ensure — the skip-if-current fast path must not preserve a
// corrupted checkout that workspaces would then inherit.
func TestEnsureHealsDirtyTree(t *testing.T) {
	setup(t)
	const name = key
	if _, err := Ensure(key, name, Ref{Short: "main"}, nil); err != nil {
		t.Fatal(err)
	}
	dir := paths.CatalogPath(name)

	// Agent mischief: make the tree writable and corrupt a tracked file.
	if err := UnlockTree(name); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("vandalized"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := Ensure(key, name, Ref{Short: "main"}, nil); err != nil {
		t.Fatalf("Ensure on a dirty catalog: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(data) != "1" {
		t.Errorf("tracked file should be restored to upstream content, got %q (err=%v)", data, err)
	}
	if treeWritable(name) {
		t.Error("healed catalog should be locked again")
	}
}
