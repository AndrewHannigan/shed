package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/config"
)

// repoTestEnv points config + data dirs at temp dirs so the list reads an
// isolated catalog.
func repoTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

// `shed repo ls` lists the repos only — never the Owners or Workspaces sections
// that top-level `shed ls` adds. That split is the whole reason the command
// exists.
func TestRepoOnlyListShowsReposOnly(t *testing.T) {
	repoTestEnv(t)
	saveConfig(t, &config.Config{
		Repos:  []config.Repo{{URL: "https://github.com/acme/widget"}},
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
	})

	out := captureStdout(t, func() {
		if err := runRepoOnlyList(false); err != nil {
			t.Fatalf("runRepoOnlyList: %v", err)
		}
	})

	if !strings.Contains(out, "github.com/acme/widget") {
		t.Errorf("repo ls output missing the repo name:\n%s", out)
	}
	// The section captions must be absent — including "Repos", which labels this
	// table only inside the `shed ls` overview, not the standalone single-table
	// view.
	for _, unwanted := range []string{"Repos", "Owners", "Workspaces"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("repo ls must not render the %q caption:\n%s", unwanted, out)
		}
	}
}

// An empty catalog shows the same actionable hint top-level `ls` shows, not
// empty headers.
func TestRepoOnlyListEmpty(t *testing.T) {
	repoTestEnv(t)
	saveConfig(t, &config.Config{})

	out := captureStdout(t, func() {
		if err := runRepoOnlyList(false); err != nil {
			t.Fatalf("runRepoOnlyList: %v", err)
		}
	})
	if !strings.Contains(out, "nothing tracked yet") {
		t.Errorf("empty catalog should show the hint:\n%s", out)
	}
}

// `shed repo ls --json` emits repos only — unlike top-level `ls --json`, with
// no owners or workspaces keys.
func TestRepoOnlyListJSONReposOnly(t *testing.T) {
	repoTestEnv(t)
	saveConfig(t, &config.Config{
		Repos:  []config.Repo{{URL: "https://github.com/acme/widget"}},
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
	})

	out := captureStdout(t, func() {
		if err := runRepoOnlyList(true); err != nil {
			t.Fatalf("runRepoOnlyList json: %v", err)
		}
	})
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("repo ls --json is not valid JSON: %v\n%s", err, out)
	}
	if _, ok := got["repos"]; !ok {
		t.Errorf("repo ls --json missing \"repos\" key:\n%s", out)
	}
	for _, key := range []string{"owners", "workspaces"} {
		if _, ok := got[key]; ok {
			t.Errorf("repo ls --json must not include a %q key:\n%s", key, out)
		}
	}
}

// repo add / repo rm change the catalog, so they are recorded in history just
// like the top-level add / rm they reuse; repo ls is read-only and is not.
func TestRepoSubcommandsRecorded(t *testing.T) {
	root := newRootCmd()
	cases := map[string]bool{
		"repo add": true,
		"repo rm":  true,
		"repo ls":  false,
	}
	for path, want := range cases {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatalf("find %q: %v", path, err)
		}
		if got := shouldRecord(cmd); got != want {
			t.Errorf("shouldRecord(%q) = %v, want %v", path, got, want)
		}
	}
}

// The `repos` and `r` aliases resolve to the same command as `repo`.
func TestRepoAliasResolves(t *testing.T) {
	for _, alias := range []string{"repos", "r"} {
		root := newRootCmd()
		cmd, _, err := root.Find([]string{alias, "ls"})
		if err != nil {
			t.Fatalf("find %s ls: %v", alias, err)
		}
		if cmd.CommandPath() != "shed repo ls" {
			t.Errorf("`%s` should alias `repo`, got command path %q", alias, cmd.CommandPath())
		}
	}
}
