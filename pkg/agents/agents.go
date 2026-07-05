// Package agents handles installing and uninstalling shed
// integration into each supported terminal coding agent.
package agents

import (
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AndrewHannigan/shed/pkg/paths"
)

// DocContent is the shed guide bundled into the binary. It is the
// body emitted by `shed __session-context` and injected into each
// agent's context via its session-context integration (a SessionStart
// hook for Claude and Cursor, the dropped-in plugin for opencode).
// Because it ships with the binary, it is always current — there is no
// installed copy to drift.
//
//go:embed embed/guide.md
var DocContent []byte

// TourContent is the welcome-tour script bundled into the binary, emitted by
// `shed __welcome-tour`. It is a set of instructions the agent reads and then
// performs as a live, narrated walkthrough of shed. Like DocContent it ships
// with the binary, so it never drifts after an upgrade.
//
//go:embed embed/welcome_tour.md
var TourContent []byte

// InstallOptions tunes a per-agent install. Most agents ignore most options.
type InstallOptions struct {
	NoBgSync bool // skip the bg-sync hook (session-context is always installed); ignored by opencode.
}

// Agent is the interface every supported agent implements.
type Agent interface {
	Key() string    // stable lower-case identifier: "claude", "cursor", ...
	Name() string   // display name: "Claude Code"
	Detected() bool // is the agent installed (config dir present)?
	Install(opts InstallOptions) (Installed, error)
	Uninstall(prev Installed) error
	// SessionContextOutput renders the shed guide body into the exact
	// shape this agent's session-context integration expects (e.g. the
	// hookSpecificOutput JSON envelope for the hook-based agents, or the raw
	// Markdown body for opencode's plugin). The content is identical across
	// agents; only the surrounding shape differs. See session_context.go.
	SessionContextOutput(body string) (string, error)
}

// Installed records what an agent's Install did, so Uninstall can
// reverse exactly those changes.
type Installed struct {
	AddedPaths []string `json:"added_paths,omitempty"`
	AddedHooks []string `json:"added_hooks,omitempty"`
	// AddedFiles are whole files shed materialized (not edits to an
	// existing settings file). Used by agents like opencode whose
	// integration is a dropped-in plugin file rather than config edits;
	// uninstall deletes exactly these.
	AddedFiles []string `json:"added_files,omitempty"`
}

// All returns the registered set of agents. New agents are added here.
func All() []Agent {
	return []Agent{
		NewClaude(),
		NewCursor(),
		NewOpencode(),
	}
}

// ByKey returns the agent with the given key, or nil.
func ByKey(key string) Agent {
	for _, a := range All() {
		if a.Key() == key {
			return a
		}
	}
	return nil
}

// SelectByFlag interprets a --agents flag value into a list of agents.
// Valid values: "auto" (only detected), "all" (every registered),
// "none" (empty), or a comma-separated list of keys.
func SelectByFlag(value string) ([]Agent, error) {
	switch value {
	case "none":
		return nil, nil
	case "all":
		return All(), nil
	case "auto":
		var out []Agent
		for _, a := range All() {
			if a.Detected() {
				out = append(out, a)
			}
		}
		return out, nil
	}
	keys := strings.Split(value, ",")
	out := make([]Agent, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		a := ByKey(k)
		if a == nil {
			return nil, fmt.Errorf("unknown agent %q (valid: %s)", k, strings.Join(allKeys(), ", "))
		}
		out = append(out, a)
	}
	return out, nil
}

func allKeys() []string {
	out := make([]string, 0)
	for _, a := range All() {
		out = append(out, a.Key())
	}
	sort.Strings(out)
	return out
}

// PathsToRegister returns the two shed directories an agent with a
// filesystem allowlist must be told it can access. Only Claude has one;
// Cursor and opencode register no paths (the chmod a-w on repos/ enforces
// read-only regardless).
func PathsToRegister() []string {
	return []string{paths.ReposDir(), paths.WorkspacesDir()}
}

// installHooks installs the hook commands every agent gets: session-context
// (always — it replaces the old @import as how the agent learns about
// shed) and bg-sync (unless --no-bg-sync). sessionContextCmd carries a
// trailing --agent <key> so the subcommand renders the output shape that agent
// expects; bgSyncCmd is the bare bg-sync command. ensure is an agent-specific
// closure that adds one hook entry for a command and reports whether it was
// newly added. Returns the commands added this call, for the install state.
func installHooks(opts InstallOptions, sessionContextCmd, bgSyncCmd string, ensure func(command string) (bool, error)) ([]string, error) {
	commands := []string{sessionContextCmd}
	if !opts.NoBgSync {
		commands = append(commands, bgSyncCmd)
	}
	var added []string
	for _, command := range commands {
		ok, err := ensure(command)
		if err != nil {
			return nil, err
		}
		if ok {
			added = append(added, command)
		}
	}
	return added, nil
}

// ErrNotDetected is returned by Install when the agent's config dir is
// missing (called for an undetected agent via --agents=all or explicit).
var ErrNotDetected = errors.New("agent not detected (config dir missing)")
