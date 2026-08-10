package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

// collectRepoList orders repos alphabetically by name, not by config order
// (which reflects when each repo was added).
func TestCollectRepoListSortsReposByName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := &config.Config{Repos: []config.Repo{
		{URL: "https://github.com/zed/zulu", Name: "github.com/zed/zulu"},
		{URL: "https://github.com/acme/widget", Name: "github.com/acme/widget"},
		{URL: "https://github.com/mid/mango", Name: "github.com/mid/mango"},
	}}
	rows, _, err := collectRepoList(c)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"github.com/acme/widget", "github.com/mid/mango", "github.com/zed/zulu"}
	if len(rows) != len(want) {
		t.Fatalf("want %d rows, got %d: %+v", len(want), len(rows), rows)
	}
	for i, name := range want {
		if rows[i].Name != name {
			t.Errorf("position %d = %q, want %q", i, rows[i].Name, name)
		}
	}
}

// writeLibrary renders a captioned section per kind of thing shed manages, so a
// newcomer can tell what each table is.
func TestWriteLibraryCaptionedSections(t *testing.T) {
	owners := []ownerRow{{Name: "octocat", RepoCount: 3}}
	repos := []repoRow{{
		Name:       "github.com/octocat/Hello-World",
		Source:     "octocat",
		Path:       "/home/u/.shed/repos/github.com/octocat/Hello-World",
		LastSyncAt: time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339),
	}}
	workspaces := []workspace.Info{{
		Name: "github.com/octocat/Hello-World", Branch: "fix-typo",
		Path: "/home/u/.shed/workspaces/x", Dirty: true, Unpushed: 2, Age: time.Now(),
	}}

	var buf bytes.Buffer
	if err := writeLibrary(&buf, owners, repos, workspaces, true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"Tracked Owners", "OWNER", "octocat",
		"Repos", "NAME", "TRACK", "SYNCED", "github.com/octocat/Hello-World",
		"Workspaces", "fix-typo", "DIRTY", "UNPUSHED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// A repo auto-added by an owner shows the FROM column.
	if !strings.Contains(out, "FROM") {
		t.Errorf("expected FROM column when a repo has a source:\n%s", out)
	}
}

// The FROM column is hidden when no repo was auto-added by an owner, so the
// common no-owners case isn't cluttered with a column of em-dashes.
func TestWriteLibraryHidesSourceColumnWhenUnused(t *testing.T) {
	repos := []repoRow{{
		Name:       "github.com/acme/widget",
		LastSyncAt: time.Now().UTC().Format(time.RFC3339),
	}}
	var buf bytes.Buffer
	if err := writeLibrary(&buf, nil, repos, nil, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "FROM") {
		t.Errorf("FROM column should be hidden when no repo has a source:\n%s", out)
	}
	// The PATH column is gone: a repo's stored-copy path is derivable from its
	// name (~/.shed/repos/<name>), so the table drops it to stay narrow.
	if strings.Contains(out, "PATH") {
		t.Errorf("PATH column should not be shown:\n%s", out)
	}
	// With no owners and no workspaces, only the Repos section appears.
	if strings.Contains(out, "Owners\n") || strings.Contains(out, "Workspaces\n") {
		t.Errorf("empty owners/workspaces sections should be omitted:\n%s", out)
	}
}

// With repos but no workspaces, the interactive hint (workspaceHint=true) keeps
// the Workspaces section and explains how to create one; the session-context
// path (workspaceHint=false) omits it to stay terse.
func TestWriteLibraryWorkspaceHint(t *testing.T) {
	repos := []repoRow{{Name: "github.com/acme/widget", LastSyncAt: time.Now().UTC().Format(time.RFC3339)}}

	var withHint bytes.Buffer
	if err := writeLibrary(&withHint, nil, repos, nil, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withHint.String(), "Workspaces\n") || !strings.Contains(withHint.String(), "workspace new") {
		t.Errorf("expected workspaces section + creation hint:\n%s", withHint.String())
	}

	var noHint bytes.Buffer
	if err := writeLibrary(&noHint, nil, repos, nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(noHint.String(), "Workspaces") {
		t.Errorf("workspaces section should be omitted with no workspaces and hint off:\n%s", noHint.String())
	}
}

// writeReposSection's caption and indent arguments are independent. The
// standalone `shed repo ls` passes ("", false): no "Repos" heading (a lone table
// needs no label) and rows flush-left. The `shed ls` overview passes ("  ", true)
// to caption the section and nest its rows two spaces under that heading.
func TestWriteReposSectionCaptionAndIndent(t *testing.T) {
	repos := []repoRow{{
		Name:       "github.com/acme/widget",
		LastSyncAt: time.Now().UTC().Format(time.RFC3339),
	}}

	// Standalone: no caption, table rows flush-left.
	var standalone bytes.Buffer
	writeReposSection(&standalone, repos, "", false)
	standaloneLines := strings.Split(strings.TrimRight(standalone.String(), "\n"), "\n")
	if standaloneLines[0] == "Repos" {
		t.Errorf("standalone view should omit the \"Repos\" caption, got:\n%s", standalone.String())
	}
	for _, line := range standaloneLines {
		if strings.HasPrefix(line, " ") {
			t.Errorf("flush-left table row should have no leading indent, got %q", line)
		}
	}

	// Overview: caption on the first line, every table row indented two spaces.
	var overview bytes.Buffer
	writeReposSection(&overview, repos, "  ", true)
	overviewLines := strings.Split(strings.TrimRight(overview.String(), "\n"), "\n")
	if overviewLines[0] != "Repos" {
		t.Errorf("expected \"Repos\" caption on the first line, got %q", overviewLines[0])
	}
	for _, line := range overviewLines[1:] {
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("nested table row should be indented two spaces, got %q", line)
		}
	}
}

// The Workspaces section lists workspaces most-recently-active first, so the
// one a user just touched sits at the top.
func TestWriteWorkspacesSectionSortsByAge(t *testing.T) {
	now := time.Now()
	infos := []workspace.Info{
		{Name: "github.com/acme/widget", Branch: "old", Path: "/w/old", Age: now.Add(-3 * time.Hour)},
		{Name: "github.com/acme/widget", Branch: "new", Path: "/w/new", Age: now.Add(-1 * time.Minute)},
		{Name: "github.com/octo/hello", Branch: "mid", Path: "/w/mid", Age: now.Add(-1 * time.Hour)},
	}

	var buf bytes.Buffer
	writeWorkspacesSection(&buf, infos)
	out := buf.String()

	iNew := strings.Index(out, "new")
	iMid := strings.Index(out, "mid")
	iOld := strings.Index(out, "old")
	if !(iNew < iMid && iMid < iOld) {
		t.Errorf("expected newest→oldest order (new, mid, old), got:\n%s", out)
	}
}

// sortWorkspacesByAge ranks newest-first without mutating the caller's slice.
func TestSortWorkspacesByAgePure(t *testing.T) {
	now := time.Now()
	infos := []workspace.Info{
		{Name: "r", Branch: "a", Age: now.Add(-2 * time.Hour)},
		{Name: "r", Branch: "b", Age: now.Add(-1 * time.Hour)},
	}
	_ = sortWorkspacesByAge(infos)
	// The original order is preserved; only the returned copy is sorted.
	if infos[0].Branch != "a" || infos[1].Branch != "b" {
		t.Errorf("input slice was mutated: %+v", infos)
	}
}

// recentWorkspaceRepos collapses workspaces to one row per repo (keeping the
// repo's newest workspace activity) and ranks them most-recent first.
func TestRecentWorkspaceRepos(t *testing.T) {
	now := time.Now()
	infos := []workspace.Info{
		{Name: "github.com/acme/widget", Branch: "a", Age: now.Add(-3 * time.Hour)},
		{Name: "github.com/acme/widget", Branch: "b", Age: now.Add(-1 * time.Hour)}, // newer; wins for this repo
		{Name: "github.com/octo/hello", Branch: "c", Age: now.Add(-2 * time.Hour)},
	}

	got := recentWorkspaceRepos(infos, 10)
	if len(got) != 2 {
		t.Fatalf("want one entry per repo (2), got %d: %+v", len(got), got)
	}
	// Ranked by each repo's newest workspace: widget's "b" (1 hr) beats hello (2 hr).
	if got[0].Name != "github.com/acme/widget" || got[1].Name != "github.com/octo/hello" {
		t.Errorf("wrong order: %+v", got)
	}
	// The repo carries its newest workspace's age, not the older sibling's.
	if !got[0].Age.Equal(now.Add(-1 * time.Hour)) {
		t.Errorf("widget should carry its newest workspace age, got %v", got[0].Age)
	}
}

// The limit keeps only the most-recently-active repos, newest first.
func TestRecentWorkspaceReposLimit(t *testing.T) {
	now := time.Now()
	var infos []workspace.Info
	for i := 0; i < 5; i++ {
		infos = append(infos, workspace.Info{
			Name: fmt.Sprintf("github.com/o/r%d", i),
			Age:  now.Add(-time.Duration(i) * time.Hour), // r0 newest … r4 oldest
		})
	}
	got := recentWorkspaceRepos(infos, 3)
	if len(got) != 3 {
		t.Fatalf("limit should cap at 3, got %d", len(got))
	}
	for i, r := range got {
		want := fmt.Sprintf("github.com/o/r%d", i)
		if r.Name != want {
			t.Errorf("position %d = %q, want %q", i, r.Name, want)
		}
	}
}

// writeRecentWorkspaceRepos renders a REPO / LAST WORKSPACE table with a
// relative age per repo.
func TestWriteRecentWorkspaceRepos(t *testing.T) {
	repos := []repoActivity{{Name: "github.com/acme/widget", Age: time.Now().Add(-2 * time.Hour)}}
	var buf bytes.Buffer
	writeRecentWorkspaceRepos(&buf, repos)
	out := buf.String()
	for _, want := range []string{"REPO", "LAST WORKSPACE", "github.com/acme/widget", "2 hr ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// An entirely empty shed shows a single actionable line, not empty headers.
func TestWriteLibraryEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := writeLibrary(&buf, nil, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "nothing tracked yet") {
		t.Errorf("expected empty-shed hint:\n%s", out)
	}
	if strings.Contains(out, "Owners") || strings.Contains(out, "Repos") || strings.Contains(out, "Workspaces") {
		t.Errorf("empty shed should not render section headers:\n%s", out)
	}
}
