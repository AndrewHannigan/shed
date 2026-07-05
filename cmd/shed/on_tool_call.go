package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/paths"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

// __on-tool-call is the pre-execution hook shed installs into each agent
// (Claude PreToolUse, Cursor beforeShellExecution, opencode plugin
// tool.execute.before). The agent hands it the session id, cwd, and the
// command about to run — all in one event. When that command creates a
// workspace (`shed workspace new` or `shed workspace from-pr`), shed records
// a pending session→workspace intent keyed by the (unique) workspace name —
// or by "pr-<number>" for a bare from-pr, whose eventual name isn't in the
// command — which the creating command then finalizes into a link sidecar.
// This is how a workspace gets tied to its session without the agent ever
// needing to know its own id.
//
// It is best-effort and MUST NOT break the agent: it always exits 0 and emits
// nothing on stdout (a silent PreToolUse hook is "allow"). Any parse failure
// just means the workspace is created unlinked.
func newOnToolCallCmd() *cobra.Command {
	var agentKey string
	cmd := &cobra.Command{
		Use:    "__on-tool-call",
		Short:  "(internal) Pre-tool hook: link a workspace to its agent session",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			recordPendingFromHook(cmd.InOrStdin(), agentKey)
			return nil
		},
	}
	cmd.Flags().StringVar(&agentKey, "agent", "", "agent whose hook JSON shape to parse (claude, cursor, opencode)")
	return cmd
}

// hookInput is the union of the fields the three agents' pre-exec hooks deliver.
// Each agent populates a different subset (see normalize()).
type hookInput struct {
	// Claude PreToolUse
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	// TranscriptPath is Claude's path to the session's JSONL transcript. Its
	// parent dir encodes (and its first entry records) the directory the session
	// was launched in — the dir `claude --resume` needs, which CWD above does not
	// reliably give (see recordPendingFromHook). Claude-only; empty elsewhere.
	TranscriptPath string `json:"transcript_path"`
	ToolInput      struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	// Cursor beforeShellExecution
	ConversationID string   `json:"conversation_id"`
	Command        string   `json:"command"`
	WorkspaceRoots []string `json:"workspace_roots"`
	// opencode (shed's plugin constructs this shape itself)
	SessionIDCamel string `json:"sessionID"`
}

// recordPendingFromHook reads the hook JSON, and if the command creates a
// workspace (new or from-pr), records a pending session→workspace intent.
func recordPendingFromHook(stdin io.Reader, agentKey string) {
	data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil || len(data) == 0 {
		return
	}
	var in hookInput
	if err := json.Unmarshal(data, &in); err != nil {
		return
	}
	sessionID, command, cwd := in.normalize()
	if sessionID == "" || command == "" {
		return
	}
	keys := parsePendingWorkspaceKeys(command)
	if len(keys) == 0 {
		return
	}
	if agentKey == "" {
		agentKey = "claude" // default mirrors __session-context
	}
	// Claude's hook `cwd` is the agent's *transient* working directory, which
	// drifts as the model cd's during a session. But a session's transcript is
	// stored under the directory it was *launched* in, and `claude --resume`
	// only finds it from that same dir — so resume must cd there, not to wherever
	// the agent happened to be when `workspace new` ran. The launch dir is
	// recorded in the transcript, so prefer it. (transcript_path is Claude-only;
	// other agents fall through to the hook cwd from normalize.)
	if agentKey == "claude" && in.TranscriptPath != "" {
		if launch, ok := launchCWDFromTranscript(in.TranscriptPath); ok {
			cwd = launch
		}
	}
	for _, key := range keys {
		_ = workspace.WritePending(key, workspace.SessionLink{
			Agent:     agentKey,
			SessionID: sessionID,
			CWD:       cwd,
		})
	}
}

// launchCWDFromTranscript returns the directory a Claude session was launched
// in, read from the first entry of its JSONL transcript that records a `cwd`.
// That first entry is the session's opening message, written from the launch
// dir — the directory whose name (munged) is the transcript's parent folder and
// the only place `claude --resume <id>` will find the session. Best-effort:
// returns ("", false) if the transcript can't be opened or has no usable cwd,
// so the caller falls back to the hook's transient cwd.
func launchCWDFromTranscript(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Transcript lines (a tool result, a pasted blob) can be large; give the
	// scanner room so the opening entry isn't skipped as an over-long token.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e struct {
			CWD string `json:"cwd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if e.CWD != "" {
			return e.CWD, true
		}
	}
	return "", false
}

// normalize collapses the per-agent field names into (sessionID, command, cwd).
func (in hookInput) normalize() (sessionID, command, cwd string) {
	sessionID = firstNonEmpty(in.SessionID, in.ConversationID, in.SessionIDCamel)
	command = firstNonEmpty(in.ToolInput.Command, in.Command)
	cwd = in.CWD
	if cwd == "" && len(in.WorkspaceRoots) > 0 {
		cwd = in.WorkspaceRoots[0]
	}
	return sessionID, command, cwd
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parsePendingWorkspaceKeys extracts the keys pending session→workspace
// intents should be recorded under, one per workspace-creating invocation in
// the command:
//
//	shed workspace new <repo> <name>          → <name>
//	shed workspace from-pr <pr> --name <name> → <name>
//	shed workspace from-pr <pr>               → prPendingKey (PR number + repo token)
//
// It tokenizes on whitespace — tolerant of shell wrappers like
// `cd x && FOO=1 shed ws new a b` — and scans for every `shed` token followed
// by `workspace`/`ws` then a creating verb, so a compound command that
// creates several workspaces records an intent for each. The bare from-pr
// form can't know the workspace's eventual name (the PR's head branch,
// resolved later via gh), so it records under the repo-scoped rendezvous key
// that runWorkspaceFromPR also checks when finalizing (see prPendingKey).
// Returns nil when the command creates no workspace.
func parsePendingWorkspaceKeys(command string) []string {
	toks := strings.Fields(command)
	var keys []string
	for i := 0; i+2 < len(toks); i++ {
		if !isShedToken(toks[i]) || (toks[i+1] != "workspace" && toks[i+1] != "ws") {
			continue
		}
		verb := toks[i+2]
		if verb != "new" && verb != "from-pr" {
			continue
		}
		if key, ok := pendingKeyFromArgs(verb, toks[i+3:]); ok {
			keys = append(keys, key)
		}
		i += 2
	}
	return dedupeStrings(keys)
}

// pendingKeyFromArgs derives one invocation's pending key from the tokens
// following its verb, scanning until a shell separator or the next shed
// invocation so a compound command's later arguments don't bleed in.
func pendingKeyFromArgs(verb string, toks []string) (string, bool) {
	var positionals []string
	var nameFlag string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if isShellSeparator(t) || isShedToken(t) {
			break
		}
		if t == "--" {
			continue
		}
		if strings.HasPrefix(t, "-") {
			// Flags that take a value: skip it (and capture --name's). Other
			// flags here are boolean; --flag=value forms are one token anyway.
			switch {
			case t == "--base":
				i++
			case t == "--name":
				if i+1 < len(toks) {
					i++
					nameFlag = toks[i]
				}
			case strings.HasPrefix(t, "--name="):
				nameFlag = strings.TrimPrefix(t, "--name=")
			}
			continue
		}
		// The PR ref is often quoted in the agent's shell command (URLs with
		// `#`); Fields keeps the quotes, so strip them.
		positionals = append(positionals, strings.Trim(t, `"'`))
		if len(positionals) == 2 {
			break
		}
	}
	var key string
	switch verb {
	case "new":
		if len(positionals) < 2 {
			return "", false
		}
		key = positionals[1]
	case "from-pr":
		key = strings.Trim(nameFlag, `"'`)
		if key == "" {
			if len(positionals) < 1 {
				return "", false
			}
			token, number, err := parsePRRef(positionals[0])
			if err != nil {
				return "", false
			}
			key = prPendingKey(token, number)
		}
	}
	// Reject anything that wouldn't be a valid workspace name, so a malformed
	// command never writes a bogus pending file.
	if err := paths.ValidateBranch(key); err != nil {
		return "", false
	}
	return key, true
}

// isShellSeparator reports whether a token separates commands in a shell
// line, ending the argument scan of the invocation before it.
func isShellSeparator(t string) bool {
	switch t {
	case "&&", "||", ";", "|", "&":
		return true
	}
	return false
}

// isShedToken reports whether a token is the shed binary invocation: bare
// "shed" or a path ending in "/shed".
func isShedToken(t string) bool {
	return t == "shed" || strings.HasSuffix(t, "/shed")
}
