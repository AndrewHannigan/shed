package main

import (
	"testing"
	"time"
)

// decidePrune gates `prune`: remove only reclaimable branches, and never
// discard local work unless --force.
func TestDecidePrune(t *testing.T) {
	tests := []struct {
		name     string
		prunable bool
		dirty    bool
		unpushed int
		force    bool
		want     pruneAction
	}{
		{"not reclaimable", false, false, 0, false, pruneKeep},
		{"not reclaimable even if dirty", false, true, 3, false, pruneKeep},
		{"reclaimable and clean", true, false, 0, false, pruneRemove},
		{"reclaimable, no upstream", true, false, -1, false, pruneRemove},
		{"reclaimable but dirty", true, true, 0, false, pruneSkip},
		{"reclaimable but unpushed", true, false, 2, false, pruneSkip},
		{"reclaimable, dirty, forced", true, true, 2, true, pruneRemove},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decidePrune(tt.prunable, tt.dirty, tt.unpushed, tt.force); got != tt.want {
				t.Errorf("decidePrune(%v, %v, %d, %v) = %d, want %d",
					tt.prunable, tt.dirty, tt.unpushed, tt.force, got, tt.want)
			}
		})
	}
}

// reclaimable decides whether a workspace's work has landed (or aged out). The
// load-bearing case: a fresh workspace whose tip merely sits on the default
// branch (landed, but no commits of its own) must NOT be reclaimable — having
// no commits beyond the default branch is not a reason to delete it.
func TestReclaimable(t *testing.T) {
	tests := []struct {
		name          string
		prNumber      int
		landed        bool
		hasOwnCommits bool
		expired       bool
		want          bool
	}{
		// The cases prune must get right (no PR, not expired). The dirty
		// variant of a fresh workspace adds nothing here — reclaimable doesn't
		// see dirtiness; decidePrune gates that — so it's covered by
		// TestDecidePrune ("not reclaimable even if dirty").
		{"fresh workspace, no commits", 0, true, false, false, false},
		{"commits, none in default branch yet", 0, false, false, false, false},
		{"commits, found in default branch", 0, true, true, false, true},
		// And the other reclaim signals:
		{"merged PR", 7, false, false, false, true},
		{"expired empty workspace", 0, true, false, true, true},
		{"expired with nothing landed", 0, false, false, true, true},
		{"nothing", 0, false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reclaimable(tt.prNumber, tt.landed, tt.hasOwnCommits, tt.expired); got != tt.want {
				t.Errorf("reclaimable(%d, %v, %v, %v) = %v, want %v",
					tt.prNumber, tt.landed, tt.hasOwnCommits, tt.expired, got, tt.want)
			}
		})
	}
}

// pruneReason reports the highest-priority reason a workspace is reclaimable.
func TestPruneReason(t *testing.T) {
	tests := []struct {
		name          string
		prNumber      int
		landed        bool
		hasOwnCommits bool
		defaultBranch string
		expired       bool
		inactive      time.Duration
		want          string
	}{
		{"merged PR wins", 12, true, true, "main", true, 100 * 24 * time.Hour, "PR #12 merged"},
		{"landed with own commits", 0, true, true, "main", false, 0, "merged into main"},
		{"landed with own commits, default unknown", 0, true, true, "", false, 0, "merged into default branch"},
		{"landed, no own commits is not a reason", 0, true, false, "main", false, 0, ""},
		{"landed, no own commits, but expired", 0, true, false, "main", true, 100 * 24 * time.Hour, "inactive for 100 days"},
		{"expired only", 0, false, false, "main", true, 100 * 24 * time.Hour, "inactive for 100 days"},
		{"nothing", 0, false, false, "main", false, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pruneReason(tt.prNumber, tt.landed, tt.hasOwnCommits, tt.defaultBranch, tt.expired, tt.inactive); got != tt.want {
				t.Errorf("pruneReason(%d, %v, %v, %q, %v, %v) = %q, want %q",
					tt.prNumber, tt.landed, tt.hasOwnCommits, tt.defaultBranch, tt.expired, tt.inactive, got, tt.want)
			}
		})
	}
}

// ghRepoFromName turns a workspace's "host/owner/repo" name into the host and
// "owner/repo" slug gh needs.
func TestGhRepoFromName(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantHost string
		wantRepo string
		wantOK   bool
	}{
		{"github", "github.com/AndrewHannigan/widgets", "github.com", "AndrewHannigan/widgets", true},
		{"enterprise host", "ghe.acme.com/team/widgets", "ghe.acme.com", "team/widgets", true},
		{"nested repo path", "github.com/acme/group/widgets", "github.com", "acme/group/widgets", true},
		{"no repo segment", "github.com/owneronly", "", "", false},
		{"host only", "github.com", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, repo, ok := ghRepoFromName(tt.in)
			if host != tt.wantHost || repo != tt.wantRepo || ok != tt.wantOK {
				t.Errorf("ghRepoFromName(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.in, host, repo, ok, tt.wantHost, tt.wantRepo, tt.wantOK)
			}
		})
	}
}

// A track-pinned repo's catalog name carries an "@<track>" suffix that is not
// part of the GitHub OWNER/REPO slug; ghRepoFromName must strip it so
// workspaces of tracked versions stay reclaimable.
func TestGhRepoFromNameStripsTrack(t *testing.T) {
	host, repo, ok := ghRepoFromName("github.com/apache/airflow@v2-7-stable")
	if !ok || host != "github.com" || repo != "apache/airflow" {
		t.Fatalf("ghRepoFromName = (%q, %q, %v), want (github.com, apache/airflow, true)", host, repo, ok)
	}
}
