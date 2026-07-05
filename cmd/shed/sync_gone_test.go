package main

import (
	"errors"
	"testing"
	"time"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/forge"
)

// TestFailAllFetchClassifiesGone verifies a vanished-remote fetch error
// becomes the distinct "gone" status for every repo of the mirror, while any
// other fetch error stays "error", so the summary and exit code can treat a
// deleted repo differently from a real fault.
func TestFailAllFetchClassifiesGone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	job := mirrorJob{key: "github.com/acme/deleted", url: "https://github.com/acme/deleted",
		repos: []syncTarget{
			{name: "github.com/acme/deleted"},
			{name: "github.com/acme/deleted@v2"},
		}}
	gone := failAllFetch(job, time.Now(),
		errors.New("git fetch: exit status 128 (output: remote: Repository not found.)"))
	if len(gone) != 2 {
		t.Fatalf("want one result per repo of the mirror, got %d", len(gone))
	}
	for _, r := range gone {
		if r.Status != "gone" {
			t.Fatalf("a not-found fetch should be gone for %s, got %q", r.Name, r.Status)
		}
	}

	job = mirrorJob{key: "github.com/acme/flaky", url: "https://github.com/acme/flaky",
		repos: []syncTarget{{name: "github.com/acme/flaky"}}}
	netErr := failAllFetch(job, time.Now(),
		errors.New("git fetch: exit status 128 (output: fatal: unable to access: connection refused)"))
	if netErr[0].Status != "error" {
		t.Fatalf("a transient fetch error should stay error, got %q", netErr[0].Status)
	}
}

// TestSummarizeSyncGoneIsNotFailure verifies "gone" repos are tallied apart
// from failures and never flip the exit code, while real network errors still
// do.
func TestSummarizeSyncGoneIsNotFailure(t *testing.T) {
	goneOnly := []syncResult{
		{Name: "a", Status: "ok"},
		{Name: "b", Status: "gone"},
	}
	if err := summarizeSync(goneOnly, len(goneOnly), true); err != nil {
		t.Fatalf("gone alone must not fail the sync, got %v", err)
	}

	withNet := []syncResult{
		{Name: "b", Status: "gone"},
		{Name: "c", Status: "error"}, // locked=false → network
	}
	err := summarizeSync(withNet, len(withNet), true)
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.Network {
		t.Fatalf("a real error alongside a gone repo should still exit %d, got %v", errs.Network, err)
	}
}

// TestReconcileOwnerKeepsAbsentRepo verifies owner reconciliation is additive
// only: a repo this owner auto-added that the listing no longer returns is left
// in config, not removed. sync surfaces it as "gone" and leaves removal to an
// explicit `shed rm` — it never deletes a tracked repo on the user's behalf.
func TestReconcileOwnerKeepsAbsentRepo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	owner := config.Owner{URL: "https://github.com/acme"}
	if err := config.Save(&config.Config{
		Owners: []config.Owner{owner},
		Repos: []config.Repo{
			{URL: "https://github.com/acme/keep", Source: "github.com/acme"},
			{URL: "https://github.com/acme/gone", Source: "github.com/acme"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The listing no longer includes "gone".
	listing := func(url string, f forge.Filter) ([]forge.Repo, error) {
		return []forge.Repo{{Name: "keep", CloneURL: "https://github.com/acme/keep"}}, nil
	}

	added, err := reconcileOwner(owner, listing)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("nothing new to add, got %v", added)
	}

	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := c.ReposForOwner("github.com/acme")
	if len(got) != 2 {
		t.Fatalf("both repos must remain (additive only), got %v", got)
	}
}
