package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/catalog"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

const testKey = "github.com/acme/widget"

// setupCatalog builds an upstream (main + feature branch "colleague" + tag
// "v1"), its mirror, and a default-branch catalog. Returns the upstream path
// and the Source for New.
func setupCatalog(t *testing.T) (string, Source) {
	t.Helper()
	if err := gitx.RequireGit(); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	git(t, root, nil, "init", "-q", "-b", "main", up)
	if err := os.WriteFile(filepath.Join(up, "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, up, nil, "add", "a.txt")
	git(t, up, nil, "commit", "-q", "-m", "first")
	git(t, up, nil, "tag", "v1")
	git(t, up, nil, "checkout", "-q", "-b", "colleague")
	if err := os.WriteFile(filepath.Join(up, "feat.txt"), []byte("wip"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, up, nil, "add", "feat.txt")
	git(t, up, nil, "commit", "-q", "-m", "colleague work")
	git(t, up, nil, "checkout", "-q", "main")

	if err := mirror.Create(up, testKey, nil); err != nil {
		t.Fatalf("mirror.Create: %v", err)
	}
	if _, err := catalog.Ensure(testKey, testKey, catalog.Ref{Short: "main"}, nil); err != nil {
		t.Fatalf("catalog.Ensure: %v", err)
	}
	return up, Source{
		Repo:      testKey,
		MirrorKey: testKey,
		Track:     "main",
		URL:       up, // the "real upstream" for this test
	}
}

// A new branch off the catalog's track: purely local clone, writable tree,
// origin re-pointed at the upstream, branch named after the workspace.
func TestNewBranchOffTrack(t *testing.T) {
	up, src := setupCatalog(t)

	path, warnings, err := New(src, "fix-thing", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if cur, _ := gitx.Output(path, "symbolic-ref", "--short", "HEAD"); cur != "fix-thing" {
		t.Errorf("workspace should be on branch fix-thing, got %q", cur)
	}
	origin, _ := gitx.Output(path, "remote", "get-url", "origin")
	if origin != up {
		t.Errorf("origin = %q, want the upstream %q", origin, up)
	}
	// A completely ordinary repo: commit and push to the (bare-ish) upstream…
	if err := os.WriteFile(filepath.Join(path, "work.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, path, nil, "add", "work.txt")
	git(t, path, nil, "commit", "-q", "-m", "work")
	git(t, path, nil, "push", "-q", "origin", "fix-thing")
	if ok, _ := gitx.RefExists(up, "refs/heads/fix-thing"); !ok {
		t.Error("push to the real upstream should work")
	}
	// …and the tree is writable (unlike a catalog).
	fi, _ := os.Stat(filepath.Join(path, "a.txt"))
	if fi.Mode().Perm()&0200 == 0 {
		t.Errorf("workspace tree should be writable, mode %v", fi.Mode())
	}
}

// A workspace named after an existing upstream branch that is NOT a catalog
// branch: the two-step path (clone + targeted fetch from the mirror), still
// offline, checking out that branch.
func TestNewExistingNonCatalogBranch(t *testing.T) {
	up, src := setupCatalog(t)

	path, _, err := New(src, "colleague", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cur, _ := gitx.Output(path, "symbolic-ref", "--short", "HEAD"); cur != "colleague" {
		t.Errorf("workspace should be on branch colleague, got %q", cur)
	}
	if _, err := os.Stat(filepath.Join(path, "feat.txt")); err != nil {
		t.Errorf("colleague's work should be checked out: %v", err)
	}
	origin, _ := gitx.Output(path, "remote", "get-url", "origin")
	if origin != up {
		t.Errorf("origin = %q, want %q", origin, up)
	}
}

// A new branch based on a tag (e.g. from a tag-tracked catalog): clone
// --branch <tag> detaches, then the new branch is created there.
func TestNewBranchOffTag(t *testing.T) {
	_, src := setupCatalog(t)

	path, _, err := New(src, "hotfix", "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cur, _ := gitx.Output(path, "symbolic-ref", "--short", "HEAD"); cur != "hotfix" {
		t.Errorf("workspace should be on branch hotfix, got %q", cur)
	}
	head, _ := gitx.RevParse(path, "HEAD")
	tag, _ := gitx.RevParse(path, "v1^{commit}")
	if head != tag {
		t.Errorf("hotfix should start at the tag: HEAD %s, v1 %s", head, tag)
	}
}

// An unknown base is a plain-language error, not a git one.
func TestNewUnknownBase(t *testing.T) {
	_, src := setupCatalog(t)
	_, _, err := New(src, "fix", "no-such-base")
	if err == nil || !strings.Contains(err.Error(), "not found upstream") {
		t.Fatalf("want a 'not found upstream' error, got %v", err)
	}
}

// A half-created leftover (origin still pointing into shed's data dir — the
// crash window between clone and remote set-url) is replaced, while a real
// workspace at the same path is refused.
func TestNewRepairsHalfCreated(t *testing.T) {
	up, src := setupCatalog(t)

	// Simulate the crash: a clone of the catalog whose origin was never
	// re-pointed.
	wsPath := PathFor(src.Repo, "crashed")
	if err := os.MkdirAll(filepath.Dir(wsPath), 0755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(wsPath), nil, "clone", "-q", paths.CatalogPath(src.Repo), wsPath)
	if !HalfCreated(src.Repo, "crashed") {
		t.Fatal("a clone with a shed-owned origin should read as half-created")
	}

	path, _, err := New(src, "crashed", "")
	if err != nil {
		t.Fatalf("New should replace the half-created leftover: %v", err)
	}
	origin, _ := gitx.Output(path, "remote", "get-url", "origin")
	if origin != up {
		t.Errorf("repaired workspace origin = %q, want %q", origin, up)
	}
	if HalfCreated(src.Repo, "crashed") {
		t.Error("repaired workspace should no longer be half-created")
	}

	// A real workspace is refused, not replaced.
	if _, _, err := New(src, "crashed", ""); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want 'already exists', got %v", err)
	}
}

// The workspace clone gets refs/remotes/origin/HEAD, which prune's
// landed-work logic depends on even after remote set-url.
func TestNewWorkspaceHasOriginHead(t *testing.T) {
	_, src := setupCatalog(t)
	path, _, err := New(src, "fix-thing", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := gitx.Output(path, "symbolic-ref", "refs/remotes/origin/HEAD"); err != nil {
		t.Errorf("workspace should have refs/remotes/origin/HEAD: %v", err)
	}
}

// An existing real workspace is never deleted by a failed re-creation: the
// second New must error out and leave the first workspace's files untouched
// (the destination is claimed atomically, so a clone failure can only ever
// clean up a directory this call created).
func TestNewNeverClobbersExistingWorkspace(t *testing.T) {
	_, src := setupCatalog(t)

	path, _, err := New(src, "fix-thing", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	precious := filepath.Join(path, "precious.txt")
	if err := os.WriteFile(precious, []byte("unsaved work"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := New(src, "fix-thing", ""); err == nil {
		t.Fatal("re-creating an existing workspace should fail")
	}
	data, err := os.ReadFile(precious)
	if err != nil || string(data) != "unsaved work" {
		t.Fatalf("existing workspace was clobbered: %q, %v", data, err)
	}
}

// A workspace whose branch is a path prefix of another's (e.g. "feature/foo"
// and "feature/foo/bar") must not be created inside the first workspace's
// working tree: the nested clone would be invisible to List and would be
// silently destroyed when the enclosing workspace is removed. New refuses it,
// and the enclosing workspace stays clean (never sees a nested .git as dirt).
func TestNewRefusesNestingUnderExistingWorkspace(t *testing.T) {
	_, src := setupCatalog(t)

	parent, _, err := New(src, "feature/foo", "")
	if err != nil {
		t.Fatalf("New(feature/foo): %v", err)
	}

	// Direct child and a deeper descendant are both refused.
	for _, nested := range []string{"feature/foo/bar", "feature/foo/a/b/c"} {
		if _, _, err := New(src, nested, ""); err == nil {
			t.Fatalf("New(%q) should be refused as nested under feature/foo", nested)
		} else if !strings.Contains(err.Error(), "feature/foo") {
			t.Errorf("New(%q) error should name the enclosing workspace, got: %v", nested, err)
		}
		if Exists(src.Repo, nested) {
			t.Errorf("refused nested workspace %q must not exist on disk", nested)
		}
	}

	// The enclosing workspace must not have been polluted by a nested clone.
	if dirty, _ := isDirty(parent); dirty {
		t.Error("enclosing workspace should stay clean (no nested .git left behind)")
	}

	// A sibling that merely shares a prefix but does not nest is still allowed,
	// as is an independent nested-name branch with no enclosing workspace.
	if _, _, err := New(src, "feature/foobar", ""); err != nil {
		t.Errorf("New(feature/foobar) sibling should be allowed: %v", err)
	}
	if _, _, err := New(src, "other/deep/one", ""); err != nil {
		t.Errorf("New(other/deep/one) with no enclosing workspace should be allowed: %v", err)
	}
}
