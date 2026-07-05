package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/agents"
	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

func newSessionContextCmd() *cobra.Command {
	var agentKey string
	var text bool
	cmd := &cobra.Command{
		Use:    "__session-context",
		Short:  "(internal) Emit the shed guide in an agent's session-context shape",
		Hidden: true,
		Long: `__session-context prints the shed guide in the shape the selected
agent's session-context integration expects, chosen with --agent <key>.
The content is identical across agents; only the surrounding shape differs,
so each agent gets what makes sense for it rather than one agent's
convention reused everywhere.

claude gets a JSON envelope it reads from its SessionStart hook and
injects into the model's context, delimited so it can be extracted from
surrounding hook output:

    <shed-session-context>{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"..."}}</shed-session-context>

opencode gets the raw Markdown body, with no envelope or delimiters: its
plugin pushes the text into the model's system prompt.

cursor gets a flat JSON object its sessionStart hook reads from stdout,
whose additional_context field is injected into the conversation:

    {"additional_context":"..."}

The guide is generated from the running binary, so it is always current —
there is no on-disk doc to drift after an upgrade. It also appends a live
snapshot of the repos the user most recently had a workspace in (capped,
newest first, with a pointer to 'shed ls' for the full library) so the
agent starts each session oriented to the active work.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --text is the deprecated alias for --agent opencode (the raw
			// body), kept so opencode plugins installed before --agent existed
			// keep working until the next `shed init`.
			if text {
				agentKey = "opencode"
			}
			return printSessionContext(os.Stdout, agentKey)
		},
	}
	// Default to claude so a bare `shed __session-context` (the hook
	// command installed before --agent existed) still emits the envelope the
	// hook-based agents accept.
	cmd.Flags().StringVar(&agentKey, "agent", "claude", "agent whose session-context output shape to emit (claude, cursor, opencode)")
	cmd.Flags().BoolVar(&text, "text", false, "deprecated alias for --agent opencode (raw guide body)")
	_ = cmd.Flags().MarkHidden("text")
	return cmd
}

// printSessionContext renders the guide body in the shape agentKey expects
// (see pkg/agents) and writes it, newline-terminated, to w.
func printSessionContext(w io.Writer, agentKey string) error {
	out, err := agents.SessionContextOutputFor(agentKey, sessionContextBody())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, out)
	return err
}

// sessionContextBody is the bundled guide followed by a live snapshot of the
// repos the user has most recently had a workspace in (so the agent starts each
// session already oriented to what they're actively working on, without having
// to run `shed ls` itself). The snapshot is best-effort: if it can't be read it
// is simply omitted.
func sessionContextBody() string {
	body := string(agents.DocContent)
	if w := syncHealthBanner(); w != "" {
		body = w + "\n" + body
	}
	if w := cwdCollisionWarning(); w != "" {
		body = w + "\n" + body
	}
	if list := recentWorkspaceReposText(); list != "" {
		body += "\nThe repos you've most recently had a workspace in (newest first; run `shed ls` for your full library):\n\n```\n" + list + "```\n"
	}
	return body
}

// syncHealthBanner returns a prominent callout when one or more tracked repos
// failed their most recent sync, so the agent treats their stored copies as
// possibly stale rather than asserting on out-of-date code. Best-effort: any
// failure to read the library yields "" and the body is emitted unchanged.
func syncHealthBanner() string {
	c, err := config.Load()
	if err != nil {
		return ""
	}
	fails := collectSyncFailures(c)
	if len(fails) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "> ⚠️ STALE STORE — %d of %d repos failed their most recent sync.\n", len(fails), len(c.Repos))
	b.WriteString("> Their stored copies are NOT current; treat anything you read from them as\n")
	b.WriteString("> possibly out of date, and tell the user:\n")
	for _, f := range fails {
		if f.LastSyncAt.IsZero() {
			fmt.Fprintf(&b, ">   - %s — never synced successfully\n", f.Name)
		} else {
			fmt.Fprintf(&b, ">   - %s — last good sync %s\n", f.Name, relTime(f.LastSyncAt))
		}
	}
	b.WriteString("> Run `shed status <repo>` for the error and the fix.\n")
	return b.String()
}

// cwdCollisionWarning returns a prominent callout when the agent's current
// working directory is itself a separate checkout of a library repo (matched
// by its git "origin" remote, protocol-independently). This is exactly the
// situation the guide's "local checkout collision" guardrail describes —
// surfaced here for the specific repo so the agent can't skip the check.
//
// Best-effort: any failure (not in a git repo, no origin, unreadable config)
// yields "" and the body is emitted unchanged. A cwd inside shed's own
// data dir (a workspace, or the read-only store) shares the upstream origin
// and is the intended place to edit, so it never warns there.
func cwdCollisionWarning() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if isWithin(cwd, paths.DataDir()) {
		return ""
	}
	origin, err := gitOriginURL(cwd)
	if err != nil {
		return ""
	}
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	return collisionWarning(cwd, origin, cfg.Repos)
}

// collisionWarning is the pure core of cwdCollisionWarning: if origin (the
// working directory's git remote) normalizes to the same host/owner/repo
// identity as one of the library repos, it returns the callout naming that
// repo; otherwise "". Both sides go through paths.DefaultName so https and
// ssh URLs for the same repo match.
func collisionWarning(cwd, origin string, repos []config.Repo) string {
	originKey, err := paths.DefaultName(origin)
	if err != nil {
		return ""
	}
	for _, r := range repos {
		key, err := paths.DefaultName(r.URL)
		if err != nil || key != originKey {
			continue
		}
		name, err := r.ResolvedName()
		if err != nil {
			name = originKey
		}
		return fmt.Sprintf("> ⚠️ HEADS UP — local checkout collision\n"+
			">\n"+
			"> Your current working directory `%s` is also library repo `%s`.\n"+
			"> They are two independent clones, and a `shed workspace` is kept\n"+
			"> up to date automatically, so it is the fresher copy. This cwd is the\n"+
			"> one genuinely ambiguous case, though — you may have been launched here\n"+
			"> on purpose to edit in place. Before editing anything here, STOP and ask\n"+
			"> the user which to use:\n"+
			">   - edit this checkout in place (it may be behind upstream), or\n"+
			">   - create an isolated, always-fresh workspace: `shed workspace new %s <branch>`\n"+
			">\n"+
			"> Do not assume — the choice decides where your commits land.\n",
			paths.Display(cwd), name, name)
	}
	return ""
}

// gitOriginURL returns the "origin" remote URL of the git repo containing dir,
// erroring if dir is not in a git repo or has no origin remote.
func gitOriginURL(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// isWithin reports whether path is dir or a descendant of it.
func isWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
