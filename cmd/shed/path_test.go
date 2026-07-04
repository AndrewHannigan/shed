package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// `shed path <workspace>` prints the writable workspace path, located by the
// globally-unique workspace name alone.
func TestRunPathWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/shed"}},
	})
	want := makeWorkspaceDir(t, "github.com/AndrewHannigan/shed", "fix-thing")

	out := captureStdout(t, func() {
		if err := runPath("fix-thing"); err != nil {
			t.Fatalf("runPath(fix-thing) = %v, want nil", err)
		}
	})
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("runPath printed %q, want workspace path %q", got, want)
	}
}

// `shed path <repo>` resolves a repo by the same shorthand the rest of shed uses
// (a trailing path segment) and prints the read-only store path.
func TestRunPathRepoByShorthand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const repo = "github.com/AndrewHannigan/projects"
	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/projects"}},
	})
	// catalog.Exists only checks the repo dir is present.
	if err := os.MkdirAll(paths.CatalogPath(repo), 0755); err != nil {
		t.Fatalf("make store dir: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runPath("projects"); err != nil {
			t.Fatalf("runPath(projects) = %v, want nil", err)
		}
	})
	if got, want := strings.TrimSpace(out), paths.CatalogPath(repo); got != want {
		t.Errorf("runPath printed %q, want repo store path %q", got, want)
	}
}

// The printed path is absolute (never a "~"), so 'cd "$(shed path <name>)"'
// works without tilde-expansion surprises.
func TestRunPathPrintsAbsolutePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/shed"}},
	})
	makeWorkspaceDir(t, "github.com/AndrewHannigan/shed", "fix-thing")

	out := captureStdout(t, func() {
		if err := runPath("fix-thing"); err != nil {
			t.Fatalf("runPath(fix-thing) = %v, want nil", err)
		}
	})
	got := strings.TrimSpace(out)
	if !strings.HasPrefix(got, "/") {
		t.Errorf("runPath should print an absolute path, got %q", got)
	}
	if strings.Contains(got, "~") {
		t.Errorf("runPath should not print a tilde, got %q", got)
	}
}

// A repo that is in the config but not yet synced (no store on disk) reports a
// NotFound with the `shed sync` fix, rather than printing a path to a missing
// directory you'd fail to cd into.
func TestRunPathRepoNotSynced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/projects"}},
	})

	err := runPath("projects")
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.NotFound {
		t.Fatalf("runPath(projects) = %v, want errs.NotFound", err)
	}
	if !strings.Contains(err.Error(), "sync") {
		t.Errorf("error should point at `shed sync`, got: %v", err)
	}
}

func TestRunPathNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/shed"}},
	})

	err := runPath("nope")
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.NotFound {
		t.Fatalf("runPath(nope) = %v, want errs.NotFound", err)
	}
}

// The namespace guards keep a name from being both a repo and a workspace, but a
// library that predates them could. path refuses such a name rather than
// guessing.
func TestRunPathAmbiguousBoth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{
			{URL: "https://github.com/AndrewHannigan/projects"},
			{URL: "https://github.com/AndrewHannigan/shed"},
		},
	})
	// A workspace literally named "projects" under a different repo, plus the
	// repo "github.com/AndrewHannigan/projects" that "projects" also resolves to
	// — the degenerate collision the guards normally prevent.
	makeWorkspaceDir(t, "github.com/AndrewHannigan/shed", "projects")

	err := runPath("projects")
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.Exists {
		t.Fatalf("runPath(projects) with a collision = %v, want errs.Exists", err)
	}
}

// Two repos sharing a leaf name under different owners are allowed. The bare
// leaf is ambiguous and the error points at the owner/repo form, which then
// resolves to exactly one.
func TestRunPathAmbiguousAcrossOwners(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{
			{URL: "https://github.com/alice/projects"},
			{URL: "https://github.com/bob/projects"},
		},
	})

	// Bare leaf can't pick one of the two owners.
	err := runPath("projects")
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.NotFound {
		t.Fatalf("runPath(projects) = %v, want errs.NotFound (ambiguous)", err)
	}
	if !strings.Contains(err.Error(), "owner/repo") {
		t.Errorf("ambiguity error should point at the owner/repo form, got: %v", err)
	}

	// The owner/repo form disambiguates to exactly one repo.
	const repo = "github.com/alice/projects"
	if mkErr := os.MkdirAll(paths.CatalogPath(repo), 0755); mkErr != nil {
		t.Fatalf("make store dir: %v", mkErr)
	}
	out := captureStdout(t, func() {
		if err := runPath("alice/projects"); err != nil {
			t.Fatalf("runPath(alice/projects) = %v, want nil", err)
		}
	})
	if got, want := strings.TrimSpace(out), paths.CatalogPath(repo); got != want {
		t.Errorf("runPath(alice/projects) = %q, want %q", got, want)
	}
}

// repoNamesMatching mirrors config.Resolve: an exact name or an unambiguous
// trailing "/"-segment selects a repo; an unknown name selects none; a shared
// leaf across hosts selects several (an ambiguous reference).
func TestRepoNamesMatching(t *testing.T) {
	c := &config.Config{Repos: []config.Repo{
		{URL: "https://github.com/AndrewHannigan/projects"},
		{URL: "https://github.com/AndrewHannigan/shed"},
		{URL: "https://gitlab.com/someone/projects"},
	}}

	if got := repoNamesMatching(c, "shed"); len(got) != 1 || got[0] != "github.com/AndrewHannigan/shed" {
		t.Errorf(`repoNamesMatching("shed") = %v, want [github.com/AndrewHannigan/shed]`, got)
	}
	if got := repoNamesMatching(c, "github.com/AndrewHannigan/shed"); len(got) != 1 {
		t.Errorf(`repoNamesMatching(full name) = %v, want one match`, got)
	}
	if got := repoNamesMatching(c, "nope"); len(got) != 0 {
		t.Errorf(`repoNamesMatching("nope") = %v, want none`, got)
	}
	if got := repoNamesMatching(c, "projects"); len(got) != 2 {
		t.Errorf(`repoNamesMatching("projects") = %v, want two (ambiguous)`, got)
	}
}

// workspaceNamesShadowedBy is the add-side mirror: it reports existing
// workspaces a newly-added repo name would shadow under `shed path`.
func TestWorkspaceNamesShadowedBy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/AndrewHannigan/shed"}},
	})
	makeWorkspaceDir(t, "github.com/AndrewHannigan/shed", "projects")
	c := loadConfig(t)

	if got := workspaceNamesShadowedBy(c, "github.com/AndrewHannigan/projects"); len(got) != 1 || got[0] != "projects" {
		t.Errorf(`workspaceNamesShadowedBy(.../projects) = %v, want [projects]`, got)
	}
	if got := workspaceNamesShadowedBy(c, "github.com/AndrewHannigan/other"); len(got) != 0 {
		t.Errorf(`workspaceNamesShadowedBy(.../other) = %v, want none`, got)
	}
}

// `workspace new` refuses a workspace name that would collide with a repo name,
// failing fast (before the network sync) so `shed path <name>` stays
// unambiguous.
func TestRunWorkspaceNewRejectsRepoNameCollision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	saveConfig(t, &config.Config{
		Repos: []config.Repo{
			{URL: "https://github.com/AndrewHannigan/projects"},
			{URL: "https://github.com/AndrewHannigan/shed"},
		},
	})

	// Make a workspace named "projects" under the shed repo; "projects" also
	// resolves to the projects repo, so the guard must reject it.
	err := runWorkspaceNew("shed", "projects", "")
	var c *errs.Coded
	if !errors.As(err, &c) || c.Code != errs.Exists {
		t.Fatalf("runWorkspaceNew(shed, projects) = %v, want errs.Exists collision", err)
	}
	if !strings.Contains(err.Error(), "collides with repo") {
		t.Errorf("error should explain the repo-name collision, got: %v", err)
	}
}
