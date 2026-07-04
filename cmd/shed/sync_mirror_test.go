package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewHannigan/shed/pkg/catalog"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// gitRun runs a git command with a pinned identity, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestSyncMirrorJobEndToEnd drives one upstream tracked three ways — default
// branch, a release branch, and a tag — through syncMirrorJob twice: the
// first pass creates the mirror once and materializes all three catalogs; the
// second pass (after an upstream commit) fast-forwards the branch catalogs
// and leaves the tag catalog untouched.
func TestSyncMirrorJobEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	gitRun(t, root, "init", "-q", "-b", "main", up)
	if err := os.WriteFile(filepath.Join(up, "a.txt"), []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, up, "add", "a.txt")
	gitRun(t, up, "commit", "-q", "-m", "first")
	gitRun(t, up, "tag", "v1")
	gitRun(t, up, "branch", "rel")

	const key = "github.com/acme/widget"
	job := mirrorJob{key: key, url: up, repos: []syncTarget{
		{name: key, url: up},
		{name: key + "@rel", url: up, track: "rel"},
		{name: key + "@v1", url: up, track: "v1"},
	}}

	results := syncMirrorJob(job, nil, 0, nil)
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d: %+v", len(results), results)
	}
	for _, r := range results {
		if r.Status != "ok" || r.Note != "created" {
			t.Errorf("%s: want ok/created, got %s/%s (%s)", r.Name, r.Status, r.Note, r.Error)
		}
	}
	// One mirror serves all three; each catalog sits on its own ref.
	if cur, _ := gitx.Output(paths.CatalogPath(key), "symbolic-ref", "--short", "HEAD"); cur != "main" {
		t.Errorf("default catalog on %q, want main", cur)
	}
	if cur, _ := gitx.Output(paths.CatalogPath(key+"@rel"), "symbolic-ref", "--short", "HEAD"); cur != "rel" {
		t.Errorf("rel catalog on %q, want rel", cur)
	}
	if _, err := gitx.Output(paths.CatalogPath(key+"@v1"), "symbolic-ref", "HEAD"); err == nil {
		t.Error("tag catalog should be detached")
	}

	// Upstream advances on main only.
	if err := os.WriteFile(filepath.Join(up, "b.txt"), []byte("2"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, up, "add", "b.txt")
	gitRun(t, up, "commit", "-q", "-m", "second")

	tagHeadBefore, _ := gitx.RevParse(paths.CatalogPath(key+"@v1"), "HEAD")
	results = syncMirrorJob(job, nil, 0, nil)
	byName := map[string]syncResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if r := byName[key]; r.Status != "ok" {
		t.Errorf("default catalog second sync: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(paths.CatalogPath(key), "b.txt")); err != nil {
		t.Errorf("default catalog should have fast-forwarded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.CatalogPath(key+"@rel"), "b.txt")); err == nil {
		t.Error("rel catalog must not receive main's commit")
	}
	tagHeadAfter, _ := gitx.RevParse(paths.CatalogPath(key+"@v1"), "HEAD")
	if tagHeadBefore != tagHeadAfter {
		t.Error("tag catalog must not move when the tag didn't")
	}

	// Meta: mirror-level fetch stamp plus one record per catalog.
	m, err := mirror.LoadMeta(key)
	if err != nil || m == nil || m.LastSyncAt.IsZero() {
		t.Fatalf("mirror meta missing after sync: %+v (%v)", m, err)
	}
	if len(m.Catalogs) != 3 {
		t.Errorf("want 3 catalog records, got %+v", m.Catalogs)
	}

	// --if-older-than: a fresh mirror skips the fetch and reports "skipped"
	// for already-current catalogs.
	results = syncMirrorJob(job, nil, time.Hour, nil)
	for _, r := range results {
		if r.Status != "skipped" {
			t.Errorf("%s: want skipped under --if-older-than, got %s (%s)", r.Name, r.Status, r.Note)
		}
	}
}

// A tracked branch deleted upstream fails with a plain-language error while
// sibling catalogs of the same mirror keep syncing.
func TestSyncMirrorJobTrackDeletedUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	gitRun(t, root, "init", "-q", "-b", "main", up)
	if err := os.WriteFile(filepath.Join(up, "a.txt"), []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, up, "add", "a.txt")
	gitRun(t, up, "commit", "-q", "-m", "first")
	gitRun(t, up, "branch", "doomed")

	const key = "github.com/acme/widget"
	job := mirrorJob{key: key, url: up, repos: []syncTarget{
		{name: key, url: up},
		{name: key + "@doomed", url: up, track: "doomed"},
	}}
	for _, r := range syncMirrorJob(job, nil, 0, nil) {
		if r.Status != "ok" {
			t.Fatalf("first sync should succeed: %+v", r)
		}
	}

	gitRun(t, up, "branch", "-D", "doomed")
	results := syncMirrorJob(job, nil, 0, nil)
	byName := map[string]syncResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if r := byName[key]; r.Status != "ok" && r.Status != "skipped" {
		t.Errorf("sibling catalog should keep syncing, got %+v", r)
	}
	r := byName[key+"@doomed"]
	if r.Status != "error" || !strings.Contains(r.Error, "no longer exists upstream") {
		t.Errorf("deleted track should fail in plain language, got %+v", r)
	}
	// The failure is recorded for status to surface.
	st := mirror.StatusFor(key, key+"@doomed")
	if st == nil || st.LastError == "" {
		t.Errorf("expected a persisted catalog error, got %+v", st)
	}
}

// An upstream with no commits yet syncs as the "empty" state — reported as
// such, no directory, no error — and materializes once upstream gains
// commits.
func TestSyncMirrorJobEmptyUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	gitRun(t, root, "init", "-q", "-b", "main", up)

	const key = "github.com/acme/empty"
	job := mirrorJob{key: key, url: up, repos: []syncTarget{{name: key, url: up}}}

	results := syncMirrorJob(job, nil, 0, nil)
	if r := results[0]; r.Status != "ok" || r.Note != "empty" {
		t.Fatalf("empty upstream should sync as ok/empty, got %+v", r)
	}
	if catalog.Exists(key) {
		t.Error("empty state should have no repo directory")
	}

	// Upstream gains a commit → the next sync materializes the catalog.
	if err := os.WriteFile(filepath.Join(up, "a.txt"), []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, up, "add", "a.txt")
	gitRun(t, up, "commit", "-q", "-m", "first")

	results = syncMirrorJob(job, nil, 0, nil)
	if r := results[0]; r.Status != "ok" || r.Note != "created" {
		t.Fatalf("first real sync should create the catalog, got %+v", r)
	}
	if !catalog.Valid(key) {
		t.Error("catalog should exist after upstream gained commits")
	}
}

// A URL that yields no safe mirror identity must fail its repos without
// touching the disk: joining a raw URL under MirrorsDir could escape it
// (mirror keys must be validated exactly like repo names).
func TestSyncMirrorJobRejectsUnsafeURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, url := range []string{
		"../../../home/user/proj",      // unparseable: raw relative path
		"https://github.com/../escape", // parseable but traversing
	} {
		jobs := groupByMirror([]syncTarget{{name: "github.com/acme/x", url: url}})
		if len(jobs) != 1 || jobs[0].keyErr == nil {
			t.Fatalf("url %q: want a job with keyErr, got %+v", url, jobs)
		}
		results := syncMirrorJob(jobs[0], nil, 0, nil)
		if len(results) != 1 || results[0].Status != "error" {
			t.Fatalf("url %q: want an error result, got %+v", url, results)
		}
	}
	// Nothing was created anywhere under the mirrors root (or outside it).
	if entries, err := os.ReadDir(paths.MirrorsDir()); err == nil && len(entries) > 0 {
		t.Fatalf("unsafe URLs must not touch the disk, found %v", entries)
	}
	if _, err := os.Stat(paths.InternalDir() + "/escape"); !os.IsNotExist(err) {
		t.Fatal("a traversing URL escaped MirrorsDir")
	}
}

// A failed fetch must not block the local phase: with the mirror already on
// disk, a newly added version still materializes from the last-synced state
// (every branch and tag from the previous fetch is local), reported as a
// failure so staleness stays visible. This is the offline `shed add
// <repo> --track <tag>` case.
func TestSyncMirrorJobOfflineMaterializesFromMirror(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	gitRun(t, root, "init", "-q", "-b", "main", up)
	if err := os.WriteFile(filepath.Join(up, "a.txt"), []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, up, "add", "a.txt")
	gitRun(t, up, "commit", "-q", "-m", "first")
	gitRun(t, up, "tag", "v1")

	const key = "github.com/acme/widget"
	// First sync online: default-branch repo only. The mirror now holds the
	// v1 tag too (forced tag refspec).
	job := mirrorJob{key: key, url: up, repos: []syncTarget{{name: key, url: up}}}
	if r := syncMirrorJob(job, nil, 0, nil)[0]; r.Status != "ok" {
		t.Fatalf("online sync should succeed: %+v", r)
	}

	// Go "offline": point the mirror's origin somewhere unreachable.
	gitRun(t, paths.MirrorPath(key), "remote", "set-url", "origin", filepath.Join(root, "nonexistent"))

	// Now the user adds a v1-pinned version and syncs, offline.
	job.repos = append(job.repos, syncTarget{name: key + "@v1", url: up, track: "v1"})
	results := syncMirrorJob(job, nil, 0, nil)
	byName := map[string]syncResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	// Both repos report the failure (the data is stale)…
	for name, r := range byName {
		if r.Status == "ok" {
			t.Errorf("%s: a failed fetch must not report ok, got %+v", name, r)
		}
	}
	// …but the new version's checkout materialized from the mirror.
	newRepo := byName[key+"@v1"]
	if !strings.Contains(newRepo.Note, "created from last-synced state") {
		t.Errorf("offline creation should be noted, got %+v", newRepo)
	}
	if !catalog.Valid(key + "@v1") {
		t.Fatal("the new version's checkout should exist offline")
	}
	if _, err := gitx.Output(paths.CatalogPath(key+"@v1"), "status"); err != nil {
		t.Errorf("offline-created checkout should be a working repo: %v", err)
	}
	// And the existing checkout was kept, not destroyed.
	if !catalog.Valid(key) {
		t.Error("the existing checkout must survive an offline sync")
	}
	// The staleness is recorded for status to surface.
	if st := mirror.StatusFor(key, key); st == nil || st.LastError == "" {
		t.Errorf("fetch failure should be recorded on the mirror, got %+v", st)
	}
}
