package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// TestFinishErrPersistsFailure verifies a failed sync records the error on the
// mirror's meta (as the repo's catalog record) while preserving the last
// *successful* sync time, and that the next successful update clears the
// recorded error.
func TestFinishErrPersistsFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const name = "github.com/acme/widget"
	const key = name // default-branch repo: name == mirror key
	if err := os.MkdirAll(filepath.Join(paths.MirrorPath(key), ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate a prior successful sync of this catalog.
	lastSync := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	if err := mirror.SaveMeta(key, &mirror.Meta{
		LastSyncAt: lastSync,
		Catalogs:   map[string]mirror.CatalogStatus{name: {LastSyncAt: lastSync}},
	}); err != nil {
		t.Fatal(err)
	}

	// A failed sync should persist the error but keep LastSyncAt untouched.
	finishErr(syncResult{Name: name}, key, time.Now(), errors.New("fetch: connection refused"))
	m := mirror.StatusFor(key, name)
	if m == nil {
		t.Fatal("expected a status record after a failed sync")
	}
	if m.LastError == "" {
		t.Fatal("expected LastError to be persisted on a failed sync")
	}
	if !m.LastSyncAt.Equal(lastSync) {
		t.Fatalf("LastSyncAt should be preserved on failure: got %v want %v", m.LastSyncAt, lastSync)
	}

	// A subsequent success clears the error.
	if err := mirror.RecordCatalogOK(key, name, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	m = mirror.StatusFor(key, name)
	if m == nil || m.LastError != "" {
		t.Fatalf("expected LastError cleared on success, got %+v", m)
	}
}

// TestFinishErrFirstCloneRecordsStandalone verifies a failure before the
// mirror exists (a failed first clone) writes no mirror meta — there's no
// mirror dir for one — but does record the error in the standalone first-sync
// store so status and the staleness banner still surface it instead of
// reporting the repo healthy.
func TestFinishErrFirstCloneRecordsStandalone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const name = "github.com/acme/never-cloned"
	finishErr(syncResult{Name: name}, name, time.Now(), errors.New("authentication failed"))

	// No mirror means no mirror meta.
	if m, _ := mirror.LoadMeta(name); m != nil {
		t.Fatalf("expected no meta written when mirror absent, got %+v", m)
	}
	// The standalone store must hold the failure.
	fe, err := mirror.LoadFirstSyncError(name)
	if err != nil || fe == nil {
		t.Fatalf("load first-sync error: %v (record=%+v)", err, fe)
	}
	if fe.LastError == "" || fe.LastErrorAt.IsZero() {
		t.Fatalf("expected first-sync error recorded with a timestamp, got %+v", fe)
	}

	// A later successful sync clears the standalone record.
	mirror.ClearFirstSyncError(name)
	if fe, _ := mirror.LoadFirstSyncError(name); fe != nil {
		t.Fatalf("expected first-sync error cleared, got %+v", fe)
	}
}

// TestMirrorFetchErrorStalesEveryCatalog verifies that a mirror-level fetch
// failure surfaces as a failure for a catalog repo of that mirror even though
// the repo's own record is clean — the shared fetch is what keeps every
// version fresh.
func TestMirrorFetchErrorStalesEveryCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const key = "github.com/acme/widget"
	const name = key + "@v2"
	if err := os.MkdirAll(filepath.Join(paths.MirrorPath(key), ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok := time.Now().Add(-1 * time.Hour).UTC()
	if err := mirror.SaveMeta(key, &mirror.Meta{
		LastSyncAt: ok,
		Catalogs:   map[string]mirror.CatalogStatus{name: {LastSyncAt: ok}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := mirror.RecordFetchError(key, "git fetch: boom"); err != nil {
		t.Fatal(err)
	}
	st := mirror.StatusFor(key, name)
	if st == nil || st.LastError == "" {
		t.Fatalf("expected the mirror fetch error to surface for the catalog, got %+v", st)
	}
	if !st.LastSyncAt.Equal(ok) {
		t.Fatalf("catalog's own LastSyncAt should be preserved, got %v", st.LastSyncAt)
	}
}

// TestRepoListMarksFailure verifies the table annotates a repo whose last
// attempt failed, without hiding the last successful sync time and without
// marking healthy repos.
func TestRepoListMarksFailure(t *testing.T) {
	rows := []repoRow{
		{Name: "github.com/acme/ok", LastSyncAt: time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
		{Name: "github.com/acme/bad", LastSyncAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339), LastError: "fetch failed"},
	}
	var buf bytes.Buffer
	writeReposSection(&buf, rows, "  ", true)
	out := buf.String()
	if !strings.Contains(out, "sync failing") {
		t.Fatalf("expected failure marker in table:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "/ok") && strings.Contains(line, "sync failing") {
			t.Fatalf("healthy repo wrongly marked:\n%s", line)
		}
	}
}
