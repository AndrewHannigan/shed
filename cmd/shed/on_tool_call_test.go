package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/workspace"
)

func TestParseWorkspaceNewName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"basic", "shed workspace new octocat/foo my-branch", "my-branch", true},
		{"ws alias", "shed ws new foo fix-bug", "fix-bug", true},
		{"with base flag", "shed workspace new foo feat --base main", "feat", true},
		{"base flag between args", "shed workspace new foo --base main feat", "feat", true},
		{"no-sync flag between args", "shed workspace new foo --no-sync feat", "feat", true},
		{"shell wrapped", "cd /tmp && FOO=1 shed ws new foo task-1", "task-1", true},
		{"abs path binary", "/usr/local/bin/shed workspace new foo task-2", "task-2", true},
		{"nested branch name", "shed workspace new foo feature/x", "feature/x", true},
		{"not a workspace new", "shed ls", "", false},
		{"workspace but not new", "shed workspace ls", "", false},
		{"only one positional", "shed workspace new foo", "", false},
		{"unrelated mentioning phrase", `echo "workspace new stuff"`, "", false},
		{"option-looking name rejected", "shed workspace new foo -evil", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseWorkspaceNewName(tt.in)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parseWorkspaceNewName(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestNormalizeHookInput(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantID  string
		wantCmd string
		wantCWD string
	}{
		{
			"claude shape",
			`{"session_id":"s1","cwd":"/a","tool_input":{"command":"shed ws new r b"}}`,
			"s1", "shed ws new r b", "/a",
		},
		{
			"cursor shape",
			`{"conversation_id":"c1","command":"shed ws new r b","workspace_roots":["/w"]}`,
			"c1", "shed ws new r b", "/w",
		},
		{
			"opencode shape",
			`{"sessionID":"ses_1","command":"shed ws new r b","cwd":"/o"}`,
			"ses_1", "shed ws new r b", "/o",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in hookInput
			if err := json.Unmarshal([]byte(tt.json), &in); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			id, cmd, cwd := in.normalize()
			if id != tt.wantID || cmd != tt.wantCmd || cwd != tt.wantCWD {
				t.Errorf("normalize() = (%q,%q,%q), want (%q,%q,%q)", id, cmd, cwd, tt.wantID, tt.wantCmd, tt.wantCWD)
			}
		})
	}
}

// recordPendingFromHook writes a pending intent for a matching workspace-new
// command, and nothing for a non-matching one.
func TestRecordPendingFromHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	recordPendingFromHook(strings.NewReader(
		`{"session_id":"sess-123","cwd":"/work/dir","tool_input":{"command":"shed workspace new foo my-task"}}`), "claude")

	got, err := workspace.TakePending("my-task")
	if err != nil {
		t.Fatalf("TakePending: %v", err)
	}
	if got == nil {
		t.Fatal("expected a pending intent for my-task, got none")
	}
	if got.Agent != "claude" || got.SessionID != "sess-123" || got.CWD != "/work/dir" {
		t.Errorf("pending = %+v, want agent=claude id=sess-123 cwd=/work/dir", *got)
	}

	// A non-workspace-new command records nothing.
	recordPendingFromHook(strings.NewReader(
		`{"session_id":"x","cwd":"/y","tool_input":{"command":"shed ls"}}`), "claude")
	if p, _ := workspace.TakePending("ls"); p != nil {
		t.Errorf("expected no pending for a non-workspace-new command, got %+v", *p)
	}
}

// For Claude, the linked cwd must be the session's *launch* dir (from the
// transcript) — not the hook's transient cwd, which drifts as the model cd's.
func TestRecordPendingFromHookClaudeUsesTranscriptLaunchCWD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Transcript whose opening entry was written from the launch dir, even
	// though the agent later cd'd elsewhere (the hook cwd below).
	transcript := writeTranscript(t,
		`{"type":"summary","summary":"x"}`, // leading non-cwd entry is skipped
		`{"cwd":"/launch/dir","sessionID":"sess-9"}`,
		`{"cwd":"/some/other/dir"}`,
	)

	recordPendingFromHook(strings.NewReader(
		`{"session_id":"sess-9","cwd":"/drifted/dir","transcript_path":`+
			jsonString(transcript)+`,"tool_input":{"command":"shed ws new foo task-9"}}`), "claude")

	got, err := workspace.TakePending("task-9")
	if err != nil || got == nil {
		t.Fatalf("TakePending: got=%v err=%v", got, err)
	}
	if got.CWD != "/launch/dir" {
		t.Errorf("CWD = %q, want the transcript launch dir /launch/dir (not the drifted hook cwd)", got.CWD)
	}
}

// When the transcript can't be read, fall back to the hook's cwd rather than
// dropping the link — resume to the wrong dir beats no resume metadata at all.
func TestRecordPendingFromHookClaudeFallsBackWhenTranscriptUnreadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	recordPendingFromHook(strings.NewReader(
		`{"session_id":"sess-x","cwd":"/hook/cwd","transcript_path":"/no/such/transcript.jsonl",`+
			`"tool_input":{"command":"shed ws new foo task-x"}}`), "claude")

	got, err := workspace.TakePending("task-x")
	if err != nil || got == nil {
		t.Fatalf("TakePending: got=%v err=%v", got, err)
	}
	if got.CWD != "/hook/cwd" {
		t.Errorf("CWD = %q, want fallback to hook cwd /hook/cwd", got.CWD)
	}
}

// transcript_path is Claude-only; a non-claude agent keeps its hook cwd even if
// a transcript_path somehow rides along.
func TestRecordPendingFromHookNonClaudeIgnoresTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	transcript := writeTranscript(t, `{"cwd":"/launch/dir"}`)
	recordPendingFromHook(strings.NewReader(
		`{"sessionID":"ses_1","cwd":"/opencode/cwd","transcript_path":`+jsonString(transcript)+
			`,"command":"shed ws new foo task-oc"}`), "opencode")

	got, err := workspace.TakePending("task-oc")
	if err != nil || got == nil {
		t.Fatalf("TakePending: got=%v err=%v", got, err)
	}
	if got.CWD != "/opencode/cwd" {
		t.Errorf("CWD = %q, want the hook cwd /opencode/cwd (transcript is Claude-only)", got.CWD)
	}
}

func TestLaunchCWDFromTranscript(t *testing.T) {
	t.Run("first cwd entry wins, leading non-cwd entries skipped", func(t *testing.T) {
		p := writeTranscript(t,
			`{"type":"summary"}`,
			`not even json`,
			`{"cwd":"/the/launch/dir"}`,
			`{"cwd":"/later/dir"}`,
		)
		got, ok := launchCWDFromTranscript(p)
		if !ok || got != "/the/launch/dir" {
			t.Errorf("launchCWDFromTranscript = (%q, %v), want (/the/launch/dir, true)", got, ok)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if got, ok := launchCWDFromTranscript("/no/such/file.jsonl"); ok {
			t.Errorf("expected (\"\", false) for a missing transcript, got (%q, true)", got)
		}
	})
	t.Run("no cwd anywhere", func(t *testing.T) {
		p := writeTranscript(t, `{"type":"summary"}`, `{"role":"user"}`)
		if got, ok := launchCWDFromTranscript(p); ok {
			t.Errorf("expected (\"\", false) when no entry has a cwd, got (%q, true)", got)
		}
	})
}

// writeTranscript writes the given JSONL lines to a temp transcript file and
// returns its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

// jsonString renders s as a JSON string literal (with surrounding quotes) for
// embedding in a hand-built hook payload.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
