package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// mkMeta creates a mirror's .git dir and writes its meta sidecar with one
// catalog record under the repo's name. The repo name doubles as the mirror
// key (the URL-derived host/owner/repo path) for these default-branch repos.
// Requires an isolated HOME (t.Setenv) so it never touches the real catalog.
func mkMeta(t *testing.T, name string, m mirror.CatalogStatus) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(paths.MirrorPath(name), ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mirror.SaveMeta(name, &mirror.Meta{
		LastSyncAt: m.LastSyncAt,
		Catalogs:   map[string]mirror.CatalogStatus{name: m},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLikelyCause(t *testing.T) {
	const https = "https://github.com/acme/widget"
	const ssh = "git@github.com:acme/widget.git"
	cases := []struct{ name, err, url, want string }{
		{"auth-https", "git fetch: exit status 128 (output: fatal: could not read Username for 'https://github.com')", https, "gh auth login"},
		{"auth-ssh", "git fetch: exit status 128 (output: git@github.com: Permission denied (publickey).)", ssh, "ssh-add"},
		{"auth-ssh-hostkey", "git fetch: exit status 128 (output: Host key verification failed.)", ssh, "SSH auth"},
		{"notfound", "git fetch: exit status 128 (output: ERROR: Repository not found.)", https, "shed rm"},
		{"network", "git fetch: exit status 128 (output: fatal: unable to access: Could not resolve host: github.com)", https, "network"},
		{"locked", "locked", https, "lock"},
		{"disk", "fatal: write error: No space left on device", https, "disk full"},
		{"unknown", "git fetch: exit status 1 (output: something unexpected)", https, "see the full output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := likelyCause(tc.err, "github.com/acme/widget", tc.url)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("likelyCause(%q) = %q, want substring %q", tc.err, got, tc.want)
			}
		})
	}
}

// status on an unknown repo must exit 2 (NotFound), consistent with every
// other repo-resolving command — not the config code 7.
func TestStatusRepoUnknownExitsNotFound(t *testing.T) {
	c := &config.Config{Repos: []config.Repo{{URL: "https://github.com/foo/bar"}}}
	err := runStatusRepo(c, "nope")
	if err == nil {
		t.Fatal("unknown repo should error")
	}
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.NotFound {
		t.Fatalf("want exit %d (NotFound), got %v", errs.NotFound, err)
	}
}

func TestCollectSyncFailuresOrdersNewestFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	c := &config.Config{Repos: []config.Repo{
		{URL: "https://github.com/acme/ok"},
		{URL: "https://github.com/acme/old-fail"},
		{URL: "https://github.com/acme/new-fail"},
	}}
	mkMeta(t, "github.com/acme/ok", mirror.CatalogStatus{LastSyncAt: time.Now()})
	mkMeta(t, "github.com/acme/old-fail", mirror.CatalogStatus{
		LastSyncAt: time.Now().Add(-48 * time.Hour), LastError: "boom", LastErrorAt: time.Now().Add(-10 * time.Hour)})
	mkMeta(t, "github.com/acme/new-fail", mirror.CatalogStatus{
		LastSyncAt: time.Now().Add(-2 * time.Hour), LastError: "boom", LastErrorAt: time.Now().Add(-1 * time.Hour)})

	got := collectSyncFailures(c)
	if len(got) != 2 {
		t.Fatalf("want 2 failures, got %d: %+v", len(got), got)
	}
	if got[0].Name != "github.com/acme/new-fail" {
		t.Fatalf("want newest failure first, got %q", got[0].Name)
	}
}

func TestSyncHealthBanner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	c := &config.Config{Repos: []config.Repo{
		{URL: "https://github.com/acme/ok"},
		{URL: "https://github.com/acme/broken"},
	}}
	if err := config.Save(c); err != nil {
		t.Fatal(err)
	}
	mkMeta(t, "github.com/acme/ok", mirror.CatalogStatus{LastSyncAt: time.Now()})
	mkMeta(t, "github.com/acme/broken", mirror.CatalogStatus{LastSyncAt: time.Now()})

	if b := syncHealthBanner(); b != "" {
		t.Fatalf("expected no banner when all healthy, got:\n%s", b)
	}

	mkMeta(t, "github.com/acme/broken", mirror.CatalogStatus{
		LastSyncAt: time.Now().Add(-3 * time.Hour), LastError: "git fetch: boom", LastErrorAt: time.Now()})
	b := syncHealthBanner()
	if !strings.Contains(b, "STALE STORE") || !strings.Contains(b, "1 of 2") {
		t.Fatalf("banner missing expected summary:\n%s", b)
	}
	if !strings.Contains(b, "github.com/acme/broken") {
		t.Fatalf("banner should name the failing repo:\n%s", b)
	}
	if strings.Contains(b, "github.com/acme/ok") {
		t.Fatalf("healthy repo should not appear in banner:\n%s", b)
	}
}

func TestWrapIndent(t *testing.T) {
	lines := wrapIndent("aaaa bbbb cccc dddd", "  ", 12)
	if len(lines) != 2 {
		t.Fatalf("want 2 wrapped lines, got %d: %q", len(lines), lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "  ") {
			t.Fatalf("line missing indent: %q", l)
		}
	}
}

// TestCollectSyncFailuresIncludesFirstSyncErrors verifies a repo that failed
// its very first clone (no store dir, so no meta sidecar) still shows up as a
// failure — the chain that feeds both `shed status` and the staleness banner.
// Without it, onboarding a private repo before auth is configured would be
// reported as "synced cleanly".
func TestCollectSyncFailuresIncludesFirstSyncErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const name = "github.com/acme/private"
	if err := mirror.RecordFirstSyncError(name, "fatal: Authentication failed"); err != nil {
		t.Fatal(err)
	}

	c := &config.Config{Repos: []config.Repo{{URL: "https://github.com/acme/private"}}}
	fails := collectSyncFailures(c)
	if len(fails) != 1 || fails[0].Name != name {
		t.Fatalf("expected first-sync failure surfaced, got %+v", fails)
	}
	// A never-succeeded repo has a zero LastSyncAt, which the banner renders as
	// "never synced successfully".
	if !fails[0].LastSyncAt.IsZero() {
		t.Fatalf("expected zero LastSyncAt for never-synced repo, got %v", fails[0].LastSyncAt)
	}

	// Once it syncs successfully the standalone record is cleared, so it no
	// longer counts as a failure.
	mirror.ClearFirstSyncError(name)
	if fails := collectSyncFailures(c); len(fails) != 0 {
		t.Fatalf("expected no failures after clear, got %+v", fails)
	}
}
