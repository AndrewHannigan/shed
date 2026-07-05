package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Claude install adds a PreToolUse hook that links workspaces to sessions, and
// the installed command is gated so it only invokes shed on a workspace-new
// match (never on every tool call).
func TestClaudeInstallAddsPreToolUseHook(t *testing.T) {
	home := withHome(t)

	got, err := NewClaude().Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	onToolCall := onToolCallCommand("claude")
	if !sliceHas(got.AddedHooks, onToolCall) {
		t.Errorf("AddedHooks = %v, want on-tool-call hook", got.AddedHooks)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	s := string(data)
	// The `if` permission-rules natively gate the hook to workspace-creating
	// commands (new and from-pr).
	for _, want := range []string{"PreToolUse", "__on-tool-call --agent claude",
		`Bash(shed workspace new *)`, `Bash(shed workspace from-pr *)`, `"if"`} {
		if !strings.Contains(s, want) {
			t.Errorf("settings.json missing %q\n%s", want, s)
		}
	}

	// Idempotent.
	again, err := NewClaude().Install(InstallOptions{})
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if sliceHas(again.AddedHooks, onToolCall) {
		t.Errorf("second Install should not re-add the hook; got %v", again.AddedHooks)
	}
}

// Uninstall removes the PreToolUse on-tool-call hook too.
func TestClaudeUninstallRemovesPreToolUseHook(t *testing.T) {
	home := withHome(t)
	c := NewClaude()

	installed, err := c.Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := c.Uninstall(installed); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if strings.Contains(string(data), "__on-tool-call") {
		t.Errorf("uninstall left on-tool-call hook behind\n%s", data)
	}
}

// Cursor install adds the beforeShellExecution hook; uninstall removes it.
func TestCursorInstallAddsBeforeShellHook(t *testing.T) {
	home := withHome(t)
	c := NewCursor()

	installed, err := c.Install(InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	for _, want := range []string{"beforeShellExecution", "__on-tool-call --agent cursor", "matcher", cursorOnToolCallMatcher} {
		if !strings.Contains(string(data), want) {
			t.Errorf("hooks.json missing %q\n%s", want, data)
		}
	}

	if err := c.Uninstall(installed); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	if strings.Contains(string(data), "__on-tool-call") {
		t.Errorf("uninstall left on-tool-call hook behind\n%s", data)
	}
}
