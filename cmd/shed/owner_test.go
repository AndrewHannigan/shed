package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/config"
)

// `shed owner ls` lists the owners only — never the Repos or Workspaces sections
// that top-level `shed ls` adds. That split is the whole reason the command
// exists (the mirror of `repo ls` from the owner side).
func TestOwnerOnlyListShowsOwnersOnly(t *testing.T) {
	repoTestEnv(t)
	saveConfig(t, &config.Config{
		Repos:  []config.Repo{{URL: "https://github.com/acme/widget"}},
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
	})

	out := captureStdout(t, func() {
		if err := runOwnerOnlyList(false); err != nil {
			t.Fatalf("runOwnerOnlyList: %v", err)
		}
	})

	if !strings.Contains(out, "github.com/acme") {
		t.Errorf("owner ls output missing the owner name:\n%s", out)
	}
	// The repo's own name must not leak in (its appearance would mean the Repos
	// section rendered); the section captions must be absent too — including
	// "Tracked Owners", which labels this table only inside the `shed ls`
	// overview, not in the standalone single-table view.
	for _, unwanted := range []string{"github.com/acme/widget", "Tracked Owners", "Repos", "Workspaces"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("owner ls must not render %q:\n%s", unwanted, out)
		}
	}
}

// With no owners, `shed owner ls` shows the owner-specific hint — even when the
// catalog has repos. nothingTrackedHint speaks to an empty catalog; an empty
// *owner* list is a different thing.
func TestOwnerOnlyListEmptyWithRepos(t *testing.T) {
	repoTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{{URL: "https://github.com/acme/widget"}},
	})

	out := captureStdout(t, func() {
		if err := runOwnerOnlyList(false); err != nil {
			t.Fatalf("runOwnerOnlyList: %v", err)
		}
	})
	if !strings.Contains(out, "no owners tracked yet") {
		t.Errorf("owner ls with no owners should show the owner hint:\n%s", out)
	}
}

// `shed owner ls --json` emits owners only — unlike top-level `ls --json`, with
// no repos or workspaces keys.
func TestOwnerOnlyListJSONOwnersOnly(t *testing.T) {
	repoTestEnv(t)
	saveConfig(t, &config.Config{
		Repos:  []config.Repo{{URL: "https://github.com/acme/widget"}},
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
	})

	out := captureStdout(t, func() {
		if err := runOwnerOnlyList(true); err != nil {
			t.Fatalf("runOwnerOnlyList json: %v", err)
		}
	})
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("owner ls --json is not valid JSON: %v\n%s", err, out)
	}
	if _, ok := got["owners"]; !ok {
		t.Errorf("owner ls --json missing \"owners\" key:\n%s", out)
	}
	for _, key := range []string{"repos", "workspaces"} {
		if _, ok := got[key]; ok {
			t.Errorf("owner ls --json must not include a %q key:\n%s", key, out)
		}
	}
}

// owner add / owner rm change the catalog, so they are recorded in history just
// like the top-level add / rm; owner ls is read-only and is not.
func TestOwnerSubcommandsRecorded(t *testing.T) {
	root := newRootCmd()
	cases := map[string]bool{
		"owner add": true,
		"owner rm":  true,
		"owner ls":  false,
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

// The `owners` and `o` aliases resolve to the same command as `owner`.
func TestOwnerAliasResolves(t *testing.T) {
	for _, alias := range []string{"owners", "o"} {
		root := newRootCmd()
		cmd, _, err := root.Find([]string{alias, "ls"})
		if err != nil {
			t.Fatalf("find %s ls: %v", alias, err)
		}
		if cmd.CommandPath() != "shed owner ls" {
			t.Errorf("`%s` should alias `owner`, got command path %q", alias, cmd.CommandPath())
		}
	}
}

// `shed owner rm` resolves names against owners only: a repo name is "not in
// the config", and an owner name removes the owner (untying its repos when run
// non-interactively).
func TestOwnerRmScopedToOwners(t *testing.T) {
	rmTestEnv(t)
	saveConfig(t, &config.Config{
		Repos: []config.Repo{
			{URL: "https://github.com/acme/a", Source: "github.com/acme"},
			{URL: "https://github.com/acme/widget"}, // user-added repo, not an owner
		},
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
	})

	// A repo name is not an owner — owner rm refuses it without touching config.
	if err := runOwnerRmByName("github.com/acme/widget", false); err == nil {
		t.Fatal("owner rm of a repo name should fail (owners-only scope)")
	} else if !strings.Contains(err.Error(), "not in the config") {
		t.Fatalf("expected an owner not-found error, got: %v", err)
	}

	// The owner name removes the owner; non-interactively its repos are untied.
	if err := runOwnerRmByName("acme", false); err != nil {
		t.Fatalf("runOwnerRmByName(owner): %v", err)
	}
	c := loadConfig(t)
	if len(c.Owners) != 0 {
		t.Fatalf("owner should be removed, got %+v", c.Owners)
	}
	if src, ok := sourceOf(t, c, "github.com/acme/a"); !ok || src != "" {
		t.Fatalf("managed repo should be kept and untied, got src=%q ok=%v", src, ok)
	}
}
