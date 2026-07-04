package mirror

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// git runs a git command in dir with a pinned identity, failing the test on
// error.
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

// makeUpstream creates a local repo with one commit on branch main and
// returns its path (usable as a clone URL).
func makeUpstream(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	git(t, root, "init", "-q", "-b", "main", up)
	if err := os.WriteFile(filepath.Join(up, "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, up, "add", "a.txt")
	git(t, up, "commit", "-q", "-m", "first")
	return up
}

const key = "github.com/acme/widget"

// Create produces the designed mirror shape: never-checked-out tree, detached
// HEAD, no local branches (the clone-created default branch is deleted so its
// name is free for a catalog worktree), the forced tag refspec, gc timing
// ownership, per-worktree config support, and the push-rejecting hook.
func TestCreateShape(t *testing.T) {
	requireGit(t)
	t.Setenv("HOME", t.TempDir())
	up := makeUpstream(t)

	if err := Create(up, key, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := paths.MirrorPath(key)

	// Working tree is empty (never checked out).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			t.Errorf("mirror working tree should be empty, found %s", e.Name())
		}
	}
	// HEAD is detached (symbolic-ref fails), and there are no local branches.
	if _, err := gitx.Output(dir, "symbolic-ref", "HEAD"); err == nil {
		t.Error("mirror HEAD should be detached")
	}
	branches, err := LocalBranches(key)
	if err != nil || len(branches) != 0 {
		t.Errorf("mirror should have no local branches, got %v (err=%v)", branches, err)
	}
	// Upstream truth lives in refs/remotes/origin/*.
	if ok, _ := gitx.RefExists(dir, "refs/remotes/origin/main"); !ok {
		t.Error("expected refs/remotes/origin/main after create")
	}
	// Creation config and the forced tag refspec.
	for cfgKey, want := range map[string]string{
		"gc.auto":                   "0",
		"extensions.worktreeConfig": "true",
	} {
		if got, _ := gitx.Output(dir, "config", cfgKey); got != want {
			t.Errorf("config %s = %q, want %q", cfgKey, got, want)
		}
	}
	out, _ := gitx.Output(dir, "config", "--get-all", "remote.origin.fetch")
	if !strings.Contains(out, "+refs/tags/*:refs/tags/*") {
		t.Errorf("missing forced tag refspec, got %q", out)
	}
	// The pre-receive hook exists and is executable.
	hook, err := os.Stat(filepath.Join(dir, ".git", "hooks", "pre-receive"))
	if err != nil || hook.Mode().Perm()&0100 == 0 {
		t.Errorf("pre-receive hook missing or not executable: %v", err)
	}
	// Idempotent.
	if err := Create(up, key, nil); err != nil {
		t.Fatalf("second Create should be a no-op, got %v", err)
	}
}

// A moved tag must not wedge the mirror: the forced tag refspec lets fetch
// clobber it, where default tag handling would fail every subsequent sync.
func TestFetchHandlesMovedAndDeletedTags(t *testing.T) {
	requireGit(t)
	t.Setenv("HOME", t.TempDir())
	up := makeUpstream(t)
	git(t, up, "tag", "v1")
	git(t, up, "tag", "doomed")

	if err := Create(up, key, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := paths.MirrorPath(key)

	// Move v1 to a new commit and delete "doomed" upstream.
	if err := os.WriteFile(filepath.Join(up, "b.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, up, "add", "b.txt")
	git(t, up, "commit", "-q", "-m", "second")
	git(t, up, "tag", "-f", "v1")
	git(t, up, "tag", "-d", "doomed")

	if err := Fetch(key, nil); err != nil {
		t.Fatalf("fetch after tag move should succeed: %v", err)
	}
	want, _ := gitx.RevParse(dir, "refs/remotes/origin/main")
	got, _ := gitx.RevParse(dir, "refs/tags/v1^{commit}")
	if got != want {
		t.Errorf("moved tag not updated: v1 at %s, main at %s", got, want)
	}
	if ok, _ := gitx.RefExists(dir, "refs/tags/doomed"); ok {
		t.Error("upstream-deleted tag should be pruned")
	}
}

// RefreshHead follows an upstream default-branch change into
// refs/remotes/origin/HEAD.
func TestRefreshHeadFollowsDefaultBranch(t *testing.T) {
	requireGit(t)
	t.Setenv("HOME", t.TempDir())
	up := makeUpstream(t)

	if err := Create(up, key, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	git(t, up, "branch", "-m", "main", "trunk")
	git(t, up, "symbolic-ref", "HEAD", "refs/heads/trunk")

	if err := Fetch(key, nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := RefreshHead(key); err != nil {
		t.Fatalf("RefreshHead: %v", err)
	}
	def, err := DefaultBranch(key)
	if err != nil || def != "trunk" {
		t.Errorf("DefaultBranch = %q (err=%v), want trunk", def, err)
	}
}

// PruneStrayBranches deletes local branches no catalog claims and leaves the
// expected set alone.
func TestPruneStrayBranches(t *testing.T) {
	requireGit(t)
	t.Setenv("HOME", t.TempDir())
	up := makeUpstream(t)
	if err := Create(up, key, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := paths.MirrorPath(key)
	git(t, dir, "branch", "keeper", "refs/remotes/origin/main")
	git(t, dir, "branch", "stray", "refs/remotes/origin/main")

	PruneStrayBranches(key, map[string]bool{"keeper": true})

	branches, _ := LocalBranches(key)
	if len(branches) != 1 || branches[0] != "keeper" {
		t.Errorf("want only keeper to survive, got %v", branches)
	}
}

// The meta status merge: a mirror-level fetch error stales every catalog; a
// catalog's own success clears only its own record.
func TestMetaLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(paths.MirrorPath(key), ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := RecordFetchOK(key, now); err != nil {
		t.Fatal(err)
	}
	const name = key + "@v2"
	// Unknown catalog on a synced mirror: no record, not "clean".
	if st := StatusFor(key, name); st != nil {
		t.Errorf("unknown catalog should have no status, got %+v", st)
	}
	if err := RecordCatalogOK(key, name, now); err != nil {
		t.Fatal(err)
	}
	st := StatusFor(key, name)
	if st == nil || st.LastError != "" || !st.LastSyncAt.Equal(now) {
		t.Fatalf("want clean record at %v, got %+v", now, st)
	}
	if err := RecordCatalogError(key, name, "boom"); err != nil {
		t.Fatal(err)
	}
	st = StatusFor(key, name)
	if st == nil || st.LastError != "boom" || !st.LastSyncAt.Equal(now) {
		t.Fatalf("error should be recorded and LastSyncAt preserved, got %+v", st)
	}
	if err := DropCatalog(key, name); err != nil {
		t.Fatal(err)
	}
	if st := StatusFor(key, name); st != nil {
		t.Errorf("dropped catalog should have no status, got %+v", st)
	}
}
