package agents

import (
	"os"
	"path/filepath"
)

const BgSyncCommand = "shed __bg-sync"

// SessionContextCommand is the base session-context subcommand. The command
// actually installed into each agent's SessionStart hook appends --agent
// <key> to it (see sessionContextCommand), so __session-context can render
// the output shape that agent expects.
const SessionContextCommand = "shed __session-context"

// OnToolCallCommand is the base pre-tool hook subcommand. Installed into each
// agent's before-tool-execution hook with --agent <key> appended (see
// onToolCallCommand), it links a workspace to the session that created it.
const OnToolCallCommand = "shed __on-tool-call"

// onToolCallCommand is the pre-tool hook command installed for an agent: the
// base __on-tool-call subcommand plus --agent <key>, so it parses that agent's
// hook JSON shape.
func onToolCallCommand(agentKey string) string {
	return OnToolCallCommand + " --agent " + agentKey
}

// Claude implements Agent for Claude Code.
type Claude struct {
	dir string // ~/.claude
}

func NewClaude() *Claude {
	home, _ := os.UserHomeDir()
	return &Claude{dir: filepath.Join(home, ".claude")}
}

func (c *Claude) Key() string  { return "claude" }
func (c *Claude) Name() string { return "Claude Code" }

func (c *Claude) Detected() bool {
	s, err := os.Stat(c.dir)
	return err == nil && s.IsDir()
}

func (c *Claude) memoryFile() string   { return filepath.Join(c.dir, "CLAUDE.md") }
func (c *Claude) settingsFile() string { return filepath.Join(c.dir, "settings.json") }

func claudeHookEntry(command string) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": command},
		},
	}
}

// claudeOnToolCallEntry builds the PreToolUse entry that links a workspace to
// its session. The matcher ("Bash") only filters on tool name, so each inner
// hook adds an `if` permission-rule that filters on the command *content* —
// Claude evaluates these natively and never runs the hook (or spawns shed) on a
// non-matching call. One `if` per accepted invocation form (`workspace new`,
// `workspace from-pr`, and their `ws` aliases).
func claudeOnToolCallEntry(command string) map[string]any {
	return map[string]any{
		"matcher": "Bash",
		"hooks": []any{
			map[string]any{"type": "command", "if": "Bash(shed workspace new *)", "command": command},
			map[string]any{"type": "command", "if": "Bash(shed ws new *)", "command": command},
			map[string]any{"type": "command", "if": "Bash(shed workspace from-pr *)", "command": command},
			map[string]any{"type": "command", "if": "Bash(shed ws from-pr *)", "command": command},
		},
	}
}

func (c *Claude) Install(opts InstallOptions) (Installed, error) {
	if err := os.MkdirAll(c.dir, 0755); err != nil {
		return Installed{}, err
	}
	paths, err := ensureArrayEntries(loadJSONC, saveJSON, c.settingsFile(),
		[]string{"permissions", "additionalDirectories"}, PathsToRegister())
	if err != nil {
		return Installed{}, err
	}
	hooks, err := installHooks(opts, sessionContextCommand(c.Key()), BgSyncCommand, func(command string) (bool, error) {
		return ensureSessionStartHook(loadJSONC, saveJSON, c.settingsFile(),
			claudeHookEntry(command), command)
	})
	if err != nil {
		return Installed{}, err
	}
	// PreToolUse hook that links a workspace to its session when the model runs
	// `shed workspace new`. The `if` permission-rules gate it natively to that
	// command, so shed is never spawned on ordinary tool calls. Separate event
	// from SessionStart, so it is installed directly rather than via installHooks.
	onToolCall := onToolCallCommand(c.Key())
	added, err := ensureHookEntry(loadJSONC, saveJSON, c.settingsFile(),
		"PreToolUse", claudeOnToolCallEntry(onToolCall), onToolCall)
	if err != nil {
		return Installed{}, err
	}
	if added {
		hooks = append(hooks, onToolCall)
	}
	return Installed{AddedPaths: paths, AddedHooks: hooks}, nil
}

func (c *Claude) Uninstall(prev Installed) error {
	if len(prev.AddedPaths) > 0 {
		if err := removeArrayEntries(loadJSONC, saveJSON, c.settingsFile(),
			[]string{"permissions", "additionalDirectories"}, prev.AddedPaths); err != nil {
			return err
		}
	}
	for _, hookCmd := range prev.AddedHooks {
		// A recorded command may live under SessionStart (session-context,
		// bg-sync) or PreToolUse (on-tool-call). removeHookEntry is a no-op for
		// an event the command isn't under, so try both.
		if err := removeSessionStartHook(loadJSONC, saveJSON, c.settingsFile(), hookCmd); err != nil {
			return err
		}
		if err := removeHookEntry(loadJSONC, saveJSON, c.settingsFile(), "PreToolUse", hookCmd); err != nil {
			return err
		}
	}
	return nil
}
