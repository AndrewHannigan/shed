package main

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/history"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// findCmd resolves a command path (e.g. "workspace", "new") against a real root
// command, so shouldRecord is exercised through the actual command tree.
func findCmd(t *testing.T, path ...string) *cobra.Command {
	t.Helper()
	root := newRootCmd()
	c, _, err := root.Find(path)
	if err != nil {
		t.Fatalf("Find(%v): %v", path, err)
	}
	return c
}

func TestShouldRecord(t *testing.T) {
	record := [][]string{
		{"add"}, {"rm"}, {"prune"}, {"init"},
		{"workspace", "new"}, {"workspace", "rm"},
	}
	skip := [][]string{
		{"sync"}, {"ls"}, {"status"}, {"history"},
		{"workspace", "ls"}, {"workspace", "path"},
		{"__bg-sync"}, {"__session-context"},
		{}, // bare root
	}
	for _, p := range record {
		if !shouldRecord(findCmd(t, p...)) {
			t.Errorf("shouldRecord(%v) = false, want true", p)
		}
	}
	for _, p := range skip {
		if shouldRecord(findCmd(t, p...)) {
			t.Errorf("shouldRecord(%v) = true, want false", p)
		}
	}
}

// Recorded history feeds the `shed history` command only — it is never
// injected into the agent's session context, even when commands are recorded.
func TestSessionContextBodyExcludesHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // empty catalog: no `ls` snapshot
	if err := os.MkdirAll(paths.DataDir(), 0755); err != nil {
		t.Fatal(err)
	}

	if err := history.Record([]string{"workspace", "new", "shed", "feat-x"}); err != nil {
		t.Fatal(err)
	}
	body := sessionContextBody()
	if strings.Contains(body, "recent `shed` commands") ||
		strings.Contains(body, "shed workspace new shed feat-x") {
		t.Errorf("session-context body should not include a recent-command history section:\n%s", body)
	}
}
