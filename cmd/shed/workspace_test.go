package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/paths"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

// `workspace ls` lists workspaces most-recently-active first, so the one a user
// just touched sits at the top — matching the Workspaces section of `shed ls`.
func TestWriteWorkspaceListTableSortsByAge(t *testing.T) {
	now := time.Now()
	infos := []workspace.Info{
		{Name: "github.com/acme/widget", Branch: "old", Path: "/w/old", Age: now.Add(-3 * time.Hour)},
		{Name: "github.com/acme/widget", Branch: "new", Path: "/w/new", Age: now.Add(-1 * time.Minute)},
		{Name: "github.com/octo/hello", Branch: "mid", Path: "/w/mid", Age: now.Add(-1 * time.Hour)},
	}

	var buf bytes.Buffer
	if err := writeWorkspaceListTable(&buf, infos); err != nil {
		t.Fatalf("writeWorkspaceListTable: %v", err)
	}
	out := buf.String()

	iNew := strings.Index(out, "new")
	iMid := strings.Index(out, "mid")
	iOld := strings.Index(out, "old")
	if !(iNew < iMid && iMid < iOld) {
		t.Errorf("expected newest→oldest order (new, mid, old), got:\n%s", out)
	}
}

// makeWorkspaceDir creates a minimal workspace dir (with a .git subdir) so
// workspace.Exists / LocateByName treat it as a real workspace.
func makeWorkspaceDir(t *testing.T, repo, name string) string {
	t.Helper()
	p := paths.WorkspacePath(repo, name)
	if err := os.MkdirAll(filepath.Join(p, ".git"), 0755); err != nil {
		t.Fatalf("make workspace dir: %v", err)
	}
	return p
}

// `workspace rm` now takes just the globally-unique workspace name and resolves
// it to the one repo it lives under, the same lookup `shed resume` uses.
func TestRunWorkspaceRmByName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/shed"}},
	})
	const repo = "github.com/AndrewHannigan/shed"
	p := makeWorkspaceDir(t, repo, "fix-thing")

	// --force skips the clean check (the bare dir isn't a real git repo).
	captureStdout(t, func() {
		if err := runWorkspaceRm("fix-thing", true); err != nil {
			t.Fatalf("runWorkspaceRm(fix-thing) = %v, want nil", err)
		}
	})
	if workspace.Exists(repo, "fix-thing") {
		t.Errorf("workspace dir %s still exists after rm", p)
	}
}

func TestRunWorkspaceRmNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/shed"}},
	})

	err := runWorkspaceRm("nope", false)
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.NotFound {
		t.Fatalf("runWorkspaceRm(nope) = %v, want errs.NotFound", err)
	}
}

// `workspace rm` accepts several names at once and removes each of them.
func TestRunWorkspaceRmManyRemovesAll(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/shed"}},
	})
	const repo = "github.com/AndrewHannigan/shed"
	makeWorkspaceDir(t, repo, "a")
	makeWorkspaceDir(t, repo, "b")
	makeWorkspaceDir(t, repo, "c")

	// --force skips the clean check (the bare dirs aren't real git repos).
	captureStdout(t, func() {
		if err := runWorkspaceRmMany([]string{"a", "c"}, true); err != nil {
			t.Fatalf("runWorkspaceRmMany(a, c) = %v, want nil", err)
		}
	})
	if workspace.Exists(repo, "a") || workspace.Exists(repo, "c") {
		t.Errorf("workspaces a and c should be removed")
	}
	if !workspace.Exists(repo, "b") {
		t.Errorf("workspace b should survive")
	}
}

// A failure on one name (a typo) does not stop the others from being removed,
// and rm reports the failure with a non-zero (NotFound) exit code.
func TestRunWorkspaceRmManyContinuesPastFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/shed"}},
	})
	const repo = "github.com/AndrewHannigan/shed"
	makeWorkspaceDir(t, repo, "a")
	makeWorkspaceDir(t, repo, "b")

	// "nope" doesn't resolve to anything; a and b do.
	var err error
	captureStdout(t, func() {
		err = runWorkspaceRmMany([]string{"a", "nope", "b"}, true)
	})
	if err == nil {
		t.Fatalf("expected an error when a name can't be removed")
	}
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.NotFound {
		t.Fatalf("runWorkspaceRmMany = %v, want errs.NotFound", err)
	}
	if workspace.Exists(repo, "a") || workspace.Exists(repo, "b") {
		t.Errorf("the resolvable workspaces should still be removed")
	}
}

// Duplicate names are collapsed so a workspace isn't removed (then reported as
// already-gone) twice — `shed ws rm a a` succeeds and removes a once.
func TestRunWorkspaceRmManyDeduplicates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/shed"}},
	})
	const repo = "github.com/AndrewHannigan/shed"
	makeWorkspaceDir(t, repo, "a")

	captureStdout(t, func() {
		if err := runWorkspaceRmMany([]string{"a", "a"}, true); err != nil {
			t.Fatalf("runWorkspaceRmMany with a duplicate name: %v", err)
		}
	})
	if workspace.Exists(repo, "a") {
		t.Errorf("workspace a should be removed")
	}
}

// `workspace new --no-sync` needs a synced checkout to fork from; on a repo
// that has never been synced it fails fast with a pointer to `shed sync`,
// never touching the network.
func TestRunWorkspaceNewNoSyncNeverSynced(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tempHome(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/acme/widget"}},
	})

	err := runWorkspaceNew("acme/widget", "feat", "", true)
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.NotFound {
		t.Fatalf("runWorkspaceNew --no-sync on never-synced repo = %v, want errs.NotFound", err)
	}
	if !strings.Contains(err.Error(), "never been synced") {
		t.Errorf("error should say the repo was never synced, got: %v", err)
	}
}

// `workspace new --no-sync` forks from the last synced state without
// refreshing: a commit added upstream after the sync must not appear in the
// workspace, where a syncing `new` would have picked it up.
func TestRunWorkspaceNewNoSyncSkipsRefresh(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tempHome(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	gitRun(t, root, "init", "-q", "-b", "main", up)
	if err := os.WriteFile(filepath.Join(up, "a.txt"), []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, up, "add", "a.txt")
	gitRun(t, up, "commit", "-q", "-m", "first")

	const key = "github.com/acme/widget"
	job := mirrorJob{key: key, url: up, repos: []syncTarget{{name: key, url: up}}}
	if r := syncMirrorJob(job, nil, 0, nil)[0]; r.Status != "ok" {
		t.Fatalf("initial sync should succeed: %+v", r)
	}
	synced, err := gitx.Output(paths.CatalogPath(key), "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read synced HEAD: %v", err)
	}

	// Upstream moves on after the sync.
	if err := os.WriteFile(filepath.Join(up, "a.txt"), []byte("2"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, up, "commit", "-aqm", "second")

	// The config URL is never contacted with --no-sync; only the already-synced
	// mirror (keyed github.com/acme/widget) is used.
	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/acme/widget"}},
	})
	captureStdout(t, func() {
		if err := runWorkspaceNew(key, "feat", "", true); err != nil {
			t.Fatalf("runWorkspaceNew --no-sync: %v", err)
		}
	})

	head, err := gitx.Output(workspace.PathFor(key, "feat"), "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read workspace HEAD: %v", err)
	}
	if strings.TrimSpace(head) != strings.TrimSpace(synced) {
		t.Errorf("--no-sync workspace HEAD = %s, want the last synced commit %s", head, synced)
	}
}
