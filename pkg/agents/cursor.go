package agents

import (
	"os"
	"path/filepath"
)

// Cursor implements Agent for Cursor's CLI agent (cursor-agent).
//
// Cursor is a SessionStart-hook agent like Claude, but its hooks file
// differs structurally: it uses the camelCase event name `hooks.sessionStart`
// with FLAT command entries ({"command": "..."}) rather than Claude's
// PascalCase `hooks.SessionStart` with nested {"hooks":[{"type","command"}]}
// entries, and it requires a top-level "version": 1. So Cursor uses its own
// hook helpers below (ensure/removeCursorHook) instead of the shared
// ensure/removeHookEntry.
//
// Like opencode, Cursor has no documented per-directory access allowlist (its
// cli-config.json permissions are shell/tool patterns, not filesystem roots),
// and the chmod a-w on repos/ enforces read-only regardless — so Cursor
// registers no paths.
type Cursor struct {
	dir string // ~/.cursor
}

func NewCursor() *Cursor {
	home, _ := os.UserHomeDir()
	return &Cursor{dir: filepath.Join(home, ".cursor")}
}

func (c *Cursor) Key() string  { return "cursor" }
func (c *Cursor) Name() string { return "Cursor CLI" }

func (c *Cursor) Detected() bool {
	s, err := os.Stat(c.dir)
	return err == nil && s.IsDir()
}

func (c *Cursor) hooksFile() string { return filepath.Join(c.dir, "hooks.json") }

func (c *Cursor) Install(opts InstallOptions) (Installed, error) {
	if err := os.MkdirAll(c.dir, 0755); err != nil {
		return Installed{}, err
	}
	hooks, err := installHooks(opts, sessionContextCommand(c.Key()), BgSyncCommand, func(command string) (bool, error) {
		return ensureCursorHook(c.hooksFile(), "sessionStart", map[string]any{"command": command}, command)
	})
	if err != nil {
		return Installed{}, err
	}
	// beforeShellExecution hook that links a workspace to its session when the
	// model runs `shed workspace new`. Cursor's per-hook `matcher` runs against
	// the shell command string, so it natively gates this to workspace-new
	// commands — shed is never spawned on ordinary shell calls. (shed re-parses
	// the command defensively, so a loose matcher just no-ops on a false match.)
	onToolCall := onToolCallCommand(c.Key())
	added, err := ensureCursorHook(c.hooksFile(), "beforeShellExecution",
		map[string]any{"command": onToolCall, "matcher": cursorOnToolCallMatcher}, onToolCall)
	if err != nil {
		return Installed{}, err
	}
	if added {
		hooks = append(hooks, onToolCall)
	}
	return Installed{AddedHooks: hooks}, nil
}

// cursorOnToolCallMatcher gates the beforeShellExecution hook to shed
// workspace-creating commands (new and from-pr). Cursor matches it
// (regex-style) against the command string.
const cursorOnToolCallMatcher = "shed (workspace|ws) (new|from-pr)"

func (c *Cursor) Uninstall(prev Installed) error {
	for _, hookCmd := range prev.AddedHooks {
		// A recorded command may live under sessionStart (session-context,
		// bg-sync) or beforeShellExecution (on-tool-call); try both.
		if err := removeCursorHook(c.hooksFile(), "sessionStart", hookCmd); err != nil {
			return err
		}
		if err := removeCursorHook(c.hooksFile(), "beforeShellExecution", hookCmd); err != nil {
			return err
		}
	}
	return nil
}

// ensureCursorHook adds `entry` (a flat object with at least a "command", and
// optionally a "matcher") to hooks.<event> in Cursor's hooks.json, creating the
// file, the nested structure, and the required top-level "version": 1 if
// missing. Idempotent: if an entry with the same command already exists under
// that event, it is a no-op. Returns true if an entry was added this call.
func ensureCursorHook(filePath, event string, entry map[string]any, command string) (bool, error) {
	root, err := loadJSONC(filePath)
	if err != nil {
		return false, err
	}
	if root == nil {
		root = map[string]any{}
	}
	if _, ok := root["version"]; !ok {
		root["version"] = 1
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	entries, _ := hooks[event].([]any)
	for _, e := range entries {
		if em, ok := e.(map[string]any); ok && em["command"] == command {
			return false, nil
		}
	}
	entries = append(entries, entry)
	hooks[event] = entries
	return true, saveJSON(filePath, root)
}

// removeCursorHook removes every hooks.<event> entry whose command equals
// command. Missing file or keys are no-ops, so it is safe to call for an event
// the command was never under.
func removeCursorHook(filePath, event, command string) error {
	root, err := loadJSONC(filePath)
	if err != nil {
		return err
	}
	if root == nil {
		return nil
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	entries, _ := hooks[event].([]any)
	if entries == nil {
		return nil
	}
	kept := entries[:0]
	for _, e := range entries {
		if em, ok := e.(map[string]any); ok && em["command"] == command {
			continue
		}
		kept = append(kept, e)
	}
	hooks[event] = kept
	return saveJSON(filePath, root)
}
