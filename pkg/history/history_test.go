package history

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/AndrewHannigan/shed/pkg/paths"
)

// setupDataDir points HOME at a temp dir and creates the shed data dir, so
// Record has somewhere to write. Returns the data dir path.
func setupDataDir(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := paths.DataDir()
	// The history file lives under .internal/; create it so tests can write
	// the file directly (Record itself also creates it on demand).
	if err := os.MkdirAll(paths.InternalDir(), 0755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	return dir
}

// Record appends, and Recent reads back oldest-first and honors the count.
func TestRecordAndRecent(t *testing.T) {
	setupDataDir(t)

	for _, args := range [][]string{
		{"add", "octocat/Hello-World"},
		{"workspace", "new", "shed", "feat-x"},
		{"prune"},
	} {
		if err := Record(args); err != nil {
			t.Fatalf("Record(%v): %v", args, err)
		}
	}

	all, err := Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}
	// Oldest first.
	if all[0].Args[0] != "add" || all[2].Args[0] != "prune" {
		t.Errorf("unexpected order: %v", all)
	}
	// Times are populated and monotonic-ish (not zero).
	if all[0].Time.IsZero() {
		t.Errorf("event time should be set")
	}

	// A smaller limit returns the most recent N.
	last2, err := Recent(2)
	if err != nil {
		t.Fatalf("Recent(2): %v", err)
	}
	if len(last2) != 2 || last2[0].Args[1] != "new" || last2[1].Args[0] != "prune" {
		t.Errorf("Recent(2) = %v, want the last two events", last2)
	}
}

// With no data dir, Record is a silent no-op and creates nothing — so it can't
// resurrect a purged data dir or materialize one on its own.
func TestRecordNoDataDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // data dir intentionally NOT created

	if err := Record([]string{"add", "x"}); err != nil {
		t.Fatalf("Record should be a no-op, got error: %v", err)
	}
	if _, err := os.Stat(paths.HistoryFile()); !os.IsNotExist(err) {
		t.Errorf("history file should not exist: %v", err)
	}
	if _, err := os.Stat(paths.DataDir()); !os.IsNotExist(err) {
		t.Errorf("data dir should not have been created: %v", err)
	}
}

// A torn or garbage line is skipped rather than failing the whole read.
func TestRecentSkipsCorruptLines(t *testing.T) {
	setupDataDir(t)
	content := `{"t":"2026-06-22T10:00:00Z","args":["add","a"]}
this is not json
{"t":"2026-06-22T10:01:00Z","args":["prune"]}
{ "t": "truncated...`
	if err := os.WriteFile(paths.HistoryFile(), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	events, err := Recent(-1)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(events) != 2 || events[0].Args[0] != "add" || events[1].Args[0] != "prune" {
		t.Errorf("expected the two valid events, got %v", events)
	}
}

// maybeTrim caps the log to maxEvents once the debounce marker is stale/absent.
func TestMaybeTrimCaps(t *testing.T) {
	setupDataDir(t)

	// Write more than the cap directly, bypassing Record's per-append trim.
	f, err := os.OpenFile(paths.HistoryFile(), os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	total := maxEvents + 50
	for i := 0; i < total; i++ {
		fmt.Fprintf(f, "{\"t\":\"2026-06-22T10:00:00Z\",\"args\":[\"n\",\"%d\"]}\n", i)
	}
	f.Close()

	// No marker → gate is open → trim runs.
	maybeTrim()

	events, err := Recent(-1)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(events) != maxEvents {
		t.Fatalf("after trim got %d events, want %d", len(events), maxEvents)
	}
	// The retained events are the most recent ones (the tail).
	if got := events[0].Args[1]; got != strconv.Itoa(total-maxEvents) {
		t.Errorf("first retained event = %s, want %d", got, total-maxEvents)
	}
	if got := events[len(events)-1].Args[1]; got != strconv.Itoa(total-1) {
		t.Errorf("last retained event = %s, want %d", got, total-1)
	}
}

// A fresh marker debounces the trim: an oversized log is left untouched until
// the interval elapses.
func TestMaybeTrimDebounced(t *testing.T) {
	setupDataDir(t)

	f, err := os.OpenFile(paths.HistoryFile(), os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	total := maxEvents + 50
	for i := 0; i < total; i++ {
		fmt.Fprintf(f, "{\"t\":\"2026-06-22T10:00:00Z\",\"args\":[\"n\",\"%d\"]}\n", i)
	}
	f.Close()

	// A recent marker means we are still within the debounce window.
	if err := os.WriteFile(paths.HistoryTrimMarkerFile(),
		[]byte(time.Now().UTC().Format(time.RFC3339)), 0644); err != nil {
		t.Fatal(err)
	}
	maybeTrim()

	events, err := Recent(-1)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(events) != total {
		t.Errorf("debounced trim should leave %d events, got %d", total, len(events))
	}
}
