package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/agents"
	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// Claude gets the hookSpecificOutput JSON envelope, wrapped in
// <shed-session-context> tags, carrying the guide as additionalContext.
func TestPrintSessionContextClaudeEnvelope(t *testing.T) {
	// Isolate from the real user config so the snapshot and the
	// collision-detection both see an empty catalog.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var buf bytes.Buffer
	if err := printSessionContext(&buf, "claude"); err != nil {
		t.Fatalf("printSessionContext: %v", err)
	}

	// Output is wrapped in <shed-session-context>...</> tags so it
	// can be extracted unambiguously from surrounding hook output.
	out := strings.TrimSuffix(buf.String(), "\n")
	if !strings.HasPrefix(out, "<shed-session-context>") || !strings.HasSuffix(out, "</shed-session-context>") {
		t.Fatalf("output should be wrapped in <shed-session-context> tags:\n%s", buf.String())
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(out, "<shed-session-context>"), "</shed-session-context>")

	var env struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(inner), &env); err != nil {
		t.Fatalf("wrapped output is not valid JSON: %v\n%s", err, inner)
	}
	if env.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", env.HookSpecificOutput.HookEventName)
	}
	if !strings.HasPrefix(env.HookSpecificOutput.AdditionalContext, string(agents.DocContent)) {
		t.Errorf("additionalContext should start with the embedded guide")
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("output should be newline-terminated")
	}
}

// opencode gets the raw Markdown body — no envelope, no delimiter tags. Its
// plugin pushes the text into the system prompt directly.
func TestPrintSessionContextOpencodeBody(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var buf bytes.Buffer
	if err := printSessionContext(&buf, "opencode"); err != nil {
		t.Fatalf("printSessionContext: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<shed-session-context>") || strings.Contains(out, "hookSpecificOutput") {
		t.Errorf("opencode output must be raw body, not the hook envelope:\n%s", out)
	}
	if !strings.HasPrefix(out, string(agents.DocContent)) {
		t.Errorf("opencode output should start with the embedded guide:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output should be newline-terminated")
	}
}

// Cursor gets a flat JSON object whose additional_context field carries the
// guide — no hookSpecificOutput envelope, no delimiter tags.
func TestPrintSessionContextCursorJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var buf bytes.Buffer
	if err := printSessionContext(&buf, "cursor"); err != nil {
		t.Fatalf("printSessionContext: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<shed-session-context>") || strings.Contains(out, "hookSpecificOutput") {
		t.Errorf("cursor output must be a flat JSON object, not the hook envelope:\n%s", out)
	}

	var obj struct {
		AdditionalContext string `json:"additional_context"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err != nil {
		t.Fatalf("cursor output is not valid JSON: %v\n%s", err, out)
	}
	if !strings.HasPrefix(obj.AdditionalContext, string(agents.DocContent)) {
		t.Errorf("additional_context should start with the embedded guide")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output should be newline-terminated")
	}
}

// An unknown --agent value is a clear error, not a silent default.
func TestPrintSessionContextUnknownAgent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var buf bytes.Buffer
	if err := printSessionContext(&buf, "nope"); err == nil {
		t.Errorf("expected error for unknown agent, got output:\n%s", buf.String())
	}
}

// With a workspace present, the body appends the recent-workspace-repos
// snapshot — naming the repo and its newest workspace's age — instead of
// dumping the whole catalog.
func TestSessionContextBodyIncludesRecentWorkspaceRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir) // isolates the workspaces dir (DataDir = $HOME/.shed)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	if err := config.Save(&config.Config{
		Repos: []config.Repo{{URL: "https://github.com/acme/widget"}},
	}); err != nil {
		t.Fatal(err)
	}
	makeGitWorkspace(t, paths.WorkspacePath("github.com/acme/widget", "fix-typo"))

	body := sessionContextBody()
	if !strings.HasPrefix(body, string(agents.DocContent)) {
		t.Fatalf("body should start with the embedded guide")
	}
	for _, want := range []string{"most recently had a workspace", "REPO", "LAST WORKSPACE", "github.com/acme/widget"} {
		if !strings.Contains(body, want) {
			t.Errorf("body should contain %q\n%s", want, body)
		}
	}
}

// With no workspaces, the body is just the guide (no snapshot noise) — the
// snapshot now keys off workspace activity, not merely a tracked repo.
func TestSessionContextBodyNoWorkspaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(&config.Config{
		Repos: []config.Repo{{URL: "https://github.com/acme/widget"}},
	}); err != nil {
		t.Fatal(err)
	}
	body := sessionContextBody()
	if !strings.HasPrefix(body, string(agents.DocContent)) {
		t.Fatalf("body should start with the embedded guide")
	}
	if strings.Contains(body, "most recently had a workspace") {
		t.Errorf("a workspace-less shed should not append a snapshot section\n%s", body)
	}
}

// makeGitWorkspace creates a minimal git repo at path so workspace.List treats
// it as a workspace (a dir containing .git) with a reflog backing its ACTIVE column.
func makeGitWorkspace(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "init")
}

// collisionWarning fires when the working directory's origin matches a catalog
// repo, regardless of URL protocol, and names the repo for `workspace new`.
func TestCollisionWarning(t *testing.T) {
	repos := []config.Repo{
		{URL: "git@github.com:octocat/hello-world.git"}, // ssh form
		{URL: "https://github.com/acme/widget"},
	}

	// https working-dir origin matches the ssh-form catalog entry.
	w := collisionWarning("/home/u/src/hello-world", "https://github.com/octocat/hello-world.git", repos)
	for _, want := range []string{
		"local checkout collision",
		"/home/u/src/hello-world",
		"workspace new github.com/octocat/hello-world <branch>",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q:\n%s", want, w)
		}
	}

	// A repo not in the catalog produces no warning.
	if w := collisionWarning("/home/u/src/other", "https://github.com/octocat/other", repos); w != "" {
		t.Errorf("expected no warning for unlisted repo, got:\n%s", w)
	}

	// The workspace command uses the catalog's resolved (custom) name.
	named := []config.Repo{{URL: "https://github.com/octocat/hello-world", Name: "myrepo"}}
	if w := collisionWarning("/x", "https://github.com/octocat/hello-world", named); !strings.Contains(w, "workspace new myrepo <branch>") {
		t.Errorf("warning should use the resolved catalog name:\n%s", w)
	}
}
