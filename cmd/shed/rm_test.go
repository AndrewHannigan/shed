package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

// rmTestEnv points config + data dirs at temp dirs so rm can mutate them, and
// redirects stdin to /dev/null so the confirmation prompts deterministically
// take their non-interactive branch (untie for owners, refuse for repos)
// regardless of how the test runner's stdin is wired.
func rmTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// A regular (empty) file is not a character device, so stdinIsTTY() reports
	// false and the prompts take their non-interactive branch. /dev/null would
	// not work here: it *is* a char device and reads as a TTY.
	f, err := os.Create(t.TempDir() + "/stdin")
	if err != nil {
		t.Fatalf("create fake stdin: %v", err)
	}
	orig := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = orig
		f.Close()
	})
}

func saveConfig(t *testing.T, c *config.Config) {
	t.Helper()
	if err := config.Save(c); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return c
}

// sourceOf returns the Source of the repo with the given resolved name, plus
// whether the repo is present at all.
func sourceOf(t *testing.T, c *config.Config, name string) (string, bool) {
	t.Helper()
	r := c.FindByName(name)
	if r == nil {
		return "", false
	}
	return r.Source, true
}

// Owner removal without --force and without a TTY keeps the repos, clearing
// their Source so they become standalone, and drops only the owner entry.
func TestOwnerRmUntiesWhenNotConfirmed(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{
			{URL: "https://github.com/acme/a", Source: "github.com/acme"},
			{URL: "https://github.com/acme/b", Source: "github.com/acme"},
			{URL: "https://github.com/acme/z"}, // user-added, not owned
		},
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
	})

	if err := runRepoRm("acme", false); err != nil {
		t.Fatalf("runRepoRm: %v", err)
	}

	c := loadConfig(t)
	if len(c.Owners) != 0 {
		t.Fatalf("owner should be removed, got %+v", c.Owners)
	}
	for _, name := range []string{"github.com/acme/a", "github.com/acme/b", "github.com/acme/z"} {
		src, ok := sourceOf(t, c, name)
		if !ok {
			t.Fatalf("repo %s should be kept", name)
		}
		if src != "" {
			t.Fatalf("repo %s should be untied (Source cleared), got %q", name, src)
		}
	}
}

// With --force, owner removal deletes the owner and every repo it managed,
// leaving user-added repos in place.
func TestOwnerRmForceRemovesManagedRepos(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{
			{URL: "https://github.com/acme/a", Source: "github.com/acme"},
			{URL: "https://github.com/acme/b", Source: "github.com/acme"},
			{URL: "https://github.com/acme/z"}, // user-added, survives
		},
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
	})

	if err := runRepoRm("acme", true); err != nil {
		t.Fatalf("runRepoRm: %v", err)
	}

	c := loadConfig(t)
	if len(c.Owners) != 0 {
		t.Fatalf("owner should be removed, got %+v", c.Owners)
	}
	if len(c.Repos) != 1 {
		t.Fatalf("only the user-added repo should remain, got %+v", c.Repos)
	}
	if _, ok := sourceOf(t, c, "github.com/acme/z"); !ok {
		t.Fatalf("user-added repo should survive owner --force removal")
	}
}

// An owner with no managed repos is dropped outright, with no prompt.
func TestOwnerRmEmptyDropsEntry(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
	})

	if err := runRepoRm("acme", false); err != nil {
		t.Fatalf("runRepoRm: %v", err)
	}

	c := loadConfig(t)
	if len(c.Owners) != 0 {
		t.Fatalf("empty owner should be removed, got %+v", c.Owners)
	}
}

// A repo with no workspaces removes cleanly without a prompt, even without a
// TTY and without --force (there is nothing to confirm).
func TestRepoRmNoWorkspacesRemovesWithoutConfirm(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/acme/z"}},
	})

	if err := runRepoRm("github.com/acme/z", false); err != nil {
		t.Fatalf("runRepoRm: %v", err)
	}

	c := loadConfig(t)
	if len(c.Repos) != 0 {
		t.Fatalf("repo should be removed, got %+v", c.Repos)
	}
}

// rm accepts several names at once and removes each of them.
func TestRepoRmManyRemovesAll(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{
			{URL: "https://github.com/acme/a"},
			{URL: "https://github.com/acme/b"},
			{URL: "https://github.com/acme/c"},
		},
	})

	if err := runRepoRmMany([]string{"github.com/acme/a", "github.com/acme/c"}, false); err != nil {
		t.Fatalf("runRepoRmMany: %v", err)
	}

	c := loadConfig(t)
	if len(c.Repos) != 1 {
		t.Fatalf("expected only one repo to remain, got %+v", c.Repos)
	}
	if _, ok := sourceOf(t, c, "github.com/acme/b"); !ok {
		t.Fatalf("unnamed repo b should survive")
	}
}

// A failure on one name does not stop the others from being removed, and rm
// reports the failure with a non-zero (NotFound) exit code.
func TestRepoRmManyContinuesPastFailure(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{
			{URL: "https://github.com/acme/a"},
			{URL: "https://github.com/acme/b"},
		},
	})

	// "nope" doesn't resolve to anything; a and b do.
	err := runRepoRmMany([]string{"github.com/acme/a", "nope", "github.com/acme/b"}, false)
	if err == nil {
		t.Fatalf("expected an error when a name can't be removed")
	}
	var coded *errs.Coded
	if !errors.As(err, &coded) {
		t.Fatalf("expected a coded error, got %T", err)
	}
	if coded.Code != errs.NotFound {
		t.Fatalf("expected NotFound exit code, got %d", coded.Code)
	}

	c := loadConfig(t)
	if len(c.Repos) != 0 {
		t.Fatalf("the resolvable repos should still be removed, got %+v", c.Repos)
	}
}

// A name that matches neither a repo nor an owner but does name a workspace
// removes just that workspace — `shed rm <ws>` is `shed workspace rm <ws>`.
func TestRmRemovesWorkspaceByName(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/acme/widget"}},
	})
	const repo = "github.com/acme/widget"
	makeWorkspaceDir(t, repo, "fix-thing")

	// --force skips the clean check (the bare dir isn't a real git repo).
	captureStdout(t, func() {
		if err := runRepoRm("fix-thing", true); err != nil {
			t.Fatalf("runRepoRm(fix-thing) = %v, want nil", err)
		}
	})
	if workspace.Exists(repo, "fix-thing") {
		t.Errorf("workspace fix-thing should be removed")
	}
	c := loadConfig(t)
	if len(c.Repos) != 1 {
		t.Fatalf("the repo should survive a workspace removal, got %+v", c.Repos)
	}
}

// A workspace may share a name with an owner (only repo names are guarded at
// creation); rm refuses to guess and removes nothing.
func TestRmWorkspaceOwnerAmbiguous(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos:  []config.Repo{{URL: "https://github.com/acme/widget", Source: "github.com/acme"}},
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
	})
	const repo = "github.com/acme/widget"
	makeWorkspaceDir(t, repo, "acme")

	err := runRepoRm("acme", false)
	if err == nil {
		t.Fatalf("expected an ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error should call out the ambiguity, got: %v", err)
	}
	if !workspace.Exists(repo, "acme") {
		t.Errorf("workspace should survive an ambiguous rm")
	}
	c := loadConfig(t)
	if len(c.Owners) != 1 {
		t.Errorf("owner should survive an ambiguous rm, got %+v", c.Owners)
	}
}

// A name matching nothing reports all three kinds rm can remove, so a
// workspace typo doesn't read as "repo not in the config".
func TestRmNotFoundMentionsWorkspaces(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/acme/widget"}},
	})

	err := runRepoRm("nope", false)
	if err == nil {
		t.Fatalf("expected a not-found error")
	}
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.NotFound {
		t.Fatalf("runRepoRm(nope) = %v, want errs.NotFound", err)
	}
	if !strings.Contains(err.Error(), "no repo, owner, or workspace") {
		t.Fatalf("not-found should mention repos, owners, and workspaces, got: %v", err)
	}
}

// Duplicate names are collapsed so a target isn't removed (then reported as
// already-gone) twice — `shed rm a a` succeeds and removes a once.
func TestRepoRmManyDeduplicates(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/acme/a"}},
	})

	if err := runRepoRmMany([]string{"github.com/acme/a", "github.com/acme/a"}, false); err != nil {
		t.Fatalf("runRepoRmMany with a duplicate name: %v", err)
	}

	c := loadConfig(t)
	if len(c.Repos) != 0 {
		t.Fatalf("repo should be removed, got %+v", c.Repos)
	}
}
