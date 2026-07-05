# Design: `shed resume` — resume an agent session from its workspace

Status: implemented (originated as a proposal on research branch `claude/session-resume-research`)
Author: research spike

## Goal

Make shed the hub for in-progress agent work. When an agent creates a
workspace mid-session, shed should record *which session* did it, so that
later you can run:

```
shed resume <name>
```

and be dropped back into that exact agent session, in the right directory,
ready to continue — across Claude Code, opencode, and Cursor, from one place.

The powerful end state: `shed ls` shows every workspace across every agent
(dirty/unpushed state, age) annotated with the session that owns it, and
`shed resume <name>` drops you back into any of them — one hub, many agents.

## Background: how each agent resumes

Researched against the agents' own docs. The two facts that matter for every
agent: **(1) how you resume from the CLI, and (2) whether resume is scoped to
the directory the session was created in.**

| | Claude Code | opencode | Cursor CLI |
|---|---|---|---|
| Resume by id | `claude --resume <id>` (`-r`) | `opencode --session <id>` (`-s`); `opencode run --session <id>` headless | `cursor-agent --resume <chatId>` |
| Continue last | `--continue` / `-c` | `--continue` / `-c` | `--continue` (= `--resume=-1`) |
| ID format | UUID | `ses_<base62>` | UUID |
| **cwd-scoped?** | **Yes** — transcript at `~/.claude/projects/<encoded-cwd>/<id>.jsonl`; wrong dir → `No conversation found` | **Yes** — project/cwd-keyed; worktrees are a known sharp edge | **No** — global store, resumes from anywhere |
| Caller-set id at creation? | Yes (`--session-id <uuid>`) | No | No (`create-chat` returns one) |
| **Min metadata to resume** | **id + cwd** | **id + cwd** | **id** (cwd advisory) |

Conclusion: **session id alone is not sufficient.** For Claude and opencode
the resume is cwd-scoped, so shed must capture *both the session id and the
directory the session was launched in*, and resume with `cd <cwd> && <agent>
<resume-flag> <id>`. For Cursor the id is enough, but shed stores the cwd
anyway so it resumes in the right worktree.

### opencode worktree caveat

opencode has several open bugs around `--continue` / `session list` picking the
wrong session across git worktrees. Since shed *is* a worktree manager, the
design must always resume by **explicit `--session <id>` from the exact dir**,
never `--continue`.

## The hard problem: correlating a workspace to a session

The naive idea — have the agent pass its own session id as a flag to
`shed workspace new --claude-code-session-id <id>` — does not work reliably,
because **no agent exposes its own session id to the shell/bash environment**.
There is no `CLAUDE_SESSION_ID` env var, no opencode/Cursor equivalent the bash
tool can read. Asking the model to pass the flag is best-effort guesswork and
will silently fail most of the time.

We *can* capture the session id at `SessionStart` (shed already runs a hook
there). But that alone doesn't solve correlation: when a later
`shed workspace new` runs, how do we know it belongs to that same session?

### Mechanisms considered

1. **Agent passes a flag** — rejected as primary; the agent can't reliably
   know its own id (see above). Kept only as an explicit override.

2. **Process-tree ancestry** — `shed workspace new` is a descendant of the
   long-lived agent process the `SessionStart` hook also ran under, so the
   common ancestor PID (guarded by start-time against PID reuse) ties them
   together. Works for Claude and Cursor (one process per session) but
   **breaks for opencode**, which runs *one shared server process across many
   concurrent sessions* — same PID, different sessions. Rejected as primary.

3. **Pre-execution hook (chosen).** Every agent has a hook that fires *before*
   a tool/shell command runs and hands the hook **the session id, the command
   string, and the cwd together in one event.** shed installs itself as that
   hook; when it sees its own `shed workspace new …` command, it records the
   workspace↔session link itself. Zero agent cooperation, uniform across all
   three, and it sidesteps opencode's shared-PID problem because the hook
   carries the real per-session id.

### The pre-execution hooks (verified against docs)

| Agent | Hook | Fields delivered together |
|---|---|---|
| Claude | `PreToolUse` (matcher `Bash`) | `session_id`, `cwd`, `tool_input.command` |
| Cursor | `beforeShellExecution` | `command`, `conversation_id`, `cwd` / `workspace_roots` |
| opencode | plugin `tool.execute.before` | `input.sessionID`, `output.args.command` (in-process plugin) |

- Claude: `session_id` is documented as exactly the id `claude --resume`
  consumes.
- Cursor / opencode: the hook-visible id (`conversation_id` / `input.sessionID`)
  is confirmed-by-chain to equal the resume id (same id flows through their
  JSON output and SDK surfaces) but is **not stated byte-for-byte in the docs**
  — needs one empirical round-trip check per agent before we depend on it.
- opencode specifics: the command is on the **second** callback arg
  (`output.args.command`), the id on the **first** (`input`); the plugin must
  read both. opencode's hook is an in-process JS/TS plugin, not a stdin-JSON
  executable like Cursor's.

## Design

### 1. Pre-execution hook (new), installed by `shed init`

Alongside the existing session-context + bg-sync hooks, shed installs a
pre-exec hook per agent. Critically, each is **natively gated to workspace-new
commands**, so the shed binary is never spawned on ordinary tool calls — only
on an actual `shed workspace new` / `shed ws new`:

- Claude: a `PreToolUse` entry (matcher `Bash`) whose inner hooks carry an
  `if` permission-rule — `Bash(shed workspace new *)` and `Bash(shed ws new *)`.
  Claude evaluates the `if` natively and runs nothing on a non-match.
- Cursor: a `beforeShellExecution` entry with a `matcher` (run against the
  shell command string): `shed (workspace|ws) new`.
- opencode: shed's in-process JS plugin gains a `tool.execute.before` handler.
  Being in-process, there is no per-call subprocess at all; a trivial
  `input.tool === "bash"` + command-substring check costs nothing and only
  shells out to `shed __on-tool-call` on a match.

The matched command is piped to `shed __on-tool-call --agent <key>` (Claude/
Cursor read the hook JSON on stdin; the opencode plugin constructs the same
JSON shape). shed re-parses the command defensively, so a loose native match
(e.g. the phrase inside an unrelated command) simply no-ops.

### 2. Linking, owned by the hook

When the hook sees a `shed workspace new` command, it has `(session_id, cwd,
command)`. The workspace doesn't exist yet at hook time (the hook fires
*before* the command runs), so linking is a two-step **pending-intent →
finalize** handshake:

1. **Hook records a pending intent.** shed owns its own CLI grammar, so it
   re-resolves the `<name>` argument with the same resolution `workspace new`
   uses and writes a small pending record keyed by that name —
   `pending/<name> = {session_id, agent, cwd}`. The globally-unique workspace
   name is the join key, so there's no cwd/ancestry guessing.
2. **`workspace new` finalizes.** On successful creation, `workspace new`
   resolves the session from the first available source — the `SHED_SESSION_*`
   env override (§6) if set, else `pending/<name>` — writes the authoritative
   link sidecar *into the new workspace* (§3), and clears the pending record.
   (The env override thus needs no hook at all: `workspace new` reads it
   directly.)

This ordering also gives the safety model for free: because the link lives
inside the workspace dir, removing the workspace removes the link automatically
(§3, §4-lifecycle) — there is no central link store to keep in sync.

- **Fallback.** The hook's parser tokenizes the command and tolerates common
  shell wrapping (`cd x && FOO=1 shed ws new …` still parses). If the name
  can't be recovered, no `pending/<name>` is written and `workspace new`
  (absent an env override) simply creates an unlinked workspace — there is no
  cwd-based fallback (resume unavailable for it — a graceful degradation,
  never an error).
- **Pending records are cleared on finalize** (`workspace new` takes and
  removes the matching record). An unmatched record is inert — a stale one
  only costs a missed link — so no TTL or age-based GC is implemented.

### 3. The link record

Written by `workspace new` *inside the workspace*, as a sidecar in its `.git/`
(e.g. `.git/shed.session`), mirroring the existing `.git/shed.meta` pattern on
stored repos:

```json
{
  "agent": "claude",
  "session_id": "550e8400-e29b-41d4-a716-446655440000",
  "cwd": "/home/user/projects/foo",
  "linked_at": "2026-06-27T19:40:00Z"
}
```

Storing it inside the workspace (rather than a central index) means it is
removed automatically when the workspace is — no separate cleanup, and the
"link never outlives or endangers its workspace" property is structural. Resume
reads it directly after `LocateByName` finds the workspace dir; no central
index to scan or keep consistent.

All three fields are always recorded — `cwd` included for every agent (the hook
supplies it authoritatively; the env override defaults it), since resume always
`cd`s to it regardless of agent.

**Cardinality (settled):**

- *Multiple workspaces per session* — fine, no special handling. Each
  `shed workspace new` makes a distinct (uniquely-named) workspace with its own
  link record pointing at the same session id; resuming any of them lands in
  that one session. It just works because the link lives per-workspace.
- *Multiple sessions per workspace* — **the creating session wins.** The link
  is written once, at creation (`workspace new` is the only writer, and it
  refuses an existing name), so a workspace later touched by a different
  session keeps its original link and resume reopens the session that created
  it. No link history; keep it straightforward.

### 4. Workspace name is the identity (not the git branch)

A workspace's identity is its **name** — the directory shed creates and owns.
The **git branch** checked out inside it is the agent's to rename, switch, or
add to; shed must never key on it. These are different things and the design
must not conflate them:

- **Workspace name** — shed-controlled, immutable for the life of the
  workspace, the directory name, the thing resume and uniqueness key on.
- **Git branch(es) inside** — agent-controlled, mutable, irrelevant to
  identity. An agent may `git checkout -b` something else mid-task; the
  workspace is still the same workspace.

shed already half-works this way: the "branch" the workspace layer tracks is
derived from the *directory name* set at creation, not from `git branch
--show-current`, so it's stable even if the agent renames the live branch.
This change makes the distinction explicit and renames the concept to
*workspace name* so nothing downstream assumes name == live branch.

`shed workspace new <repo> <name>` creates a workspace named `<name>` and, as a
convenience, seeds an initial git branch of the same name — but that's just the
starting point, not a binding. (A later `--branch` option could decouple the
seed branch from the name; not needed now.)

#### Uniqueness invariant → resume by name alone

> **A workspace name is unique across the entire shed.** `shed workspace new
> <repo> <name>` fails if a workspace named `<name>` already exists under *any*
> repo, naming the conflict.

The on-disk layout stays `<repo>/<name>` (so the path still encodes the repo),
but the name alone is now a key, so `shed resume <name>` resolves to exactly
one workspace — no `<repo>` needed. A `workspace.LocateByName(name)` lookup
backs both the creation guard and resume.

Tradeoff: no two live workspaces can share a name — most visibly if you tried
two `main` workspaces across repos. This fits shed's grain (workspaces are
named per task, e.g. `fix-readme-link`), and the failure is loud and actionable
("workspace `main` already exists for `other/repo`; pick a distinct name").
The same invariant lets `shed path` / `workspace rm` accept name-only too —
both do, via the same lookup.

### 5. `shed resume`

`shed resume` requires exactly one workspace-name argument (enforced by
hand so a `-- <agent args>` passthrough still parses). There is no
bare/no-args form; listing in-progress work is
`shed ls`'s job (which already shows workspaces, and can later be annotated
with the linked session/agent). Bare `shed resume` errors with usage.

**`shed resume <name>`** → resolve the unique workspace for `<name>`, read its
link, then exec:

  ```
  cd <cwd> && <agent-bin> <resume-flag> <session_id> <args-after-->
  ```

  Resume flags: `claude --resume` · `opencode --session` · `cursor-agent --resume`.

#### Argument contract

```
shed resume <name> [shed flags] [-- <args passed straight to the agent>]
```

Everything after `--` is appended to the agent invocation **verbatim** — shed
never interprets it, so each agent's own prompt/print/flag conventions just
work and stay robust as those agents add flags. cobra stops flag parsing at
`--` automatically, so shed's own flags must precede it.

- Interactive: `shed resume fix-bug` → `cd <cwd> && claude --resume <id>`
- Non-interactive: `shed resume fix-bug -- -p "continue the refactor"` →
  `cd <cwd> && claude --resume <id> -p "continue the refactor"`
- Dry run: `shed resume fix-bug --print` emits the command instead of exec'ing
  (note `--print` is before `--`).

This makes automation (cron, CI, a parent agent fanning out resumes) use the
exact same path as interactive use.

**resume always `cd`s to the session's wd, for every agent — including Cursor.**
Cursor's resume isn't cwd-scoped (it would work from anywhere), but dropping the
resumed session into its original worktree is strictly better (the agent picks
up where its files are) and has no downside: the `cd` happens inside shed's own
subprocess, so it never changes your parent shell's directory (see "resume is a
launcher, not a cd", below). So the per-agent cwd-scoping distinction only
governs whether the `cd` is *strictly necessary* for the transcript lookup
(Claude/opencode) — not whether we do it. We always do.

### resume is a launcher, not a `cd`

`shed resume` does **not** change your shell's working directory. A child
process can't alter its parent's cwd; shed `chdir`s only within its own process
to launch the agent in the right place, and when the agent session ends you are
returned to wherever you ran `shed resume`. (`--print` is the exception by
design: it emits `cd <cwd> && …` for you to inspect or eval yourself.)

### 6. Override via environment, not a visible flag

The session→workspace link is normally established invisibly by the pre-exec
hook (§1–§2), so **the agent's canonical command stays exactly `shed workspace
new <repo> <name>`** — no session argument, nothing for the model to fill in.
This matters: a visible `--…-session-id` flag in `--help` would pressure the
agent to supply an id it cannot reliably know, so we deliberately do **not**
expose one.

For headless / no-hook contexts (e.g. `claude -p` runs, CI) where the pre-exec
hook may not fire, the override is an **environment variable**, set once by the
orchestrator that drives the agent (and which already knows the id — it minted
it via `claude --session-id $ID`). The agent still runs the plain command;
shed reads the environment:

| Env var | Required? | Meaning |
|---|---|---|
| `SHED_SESSION_ID` | **yes** | the session/chat id to link (shed can't derive it) |
| `SHED_SESSION_AGENT` | **yes** | which agent (`claude`/`opencode`/`cursor`) — selects the resume command (shed can't derive it) |
| `SHED_SESSION_CWD` | optional | the session's launch dir; defaults to the directory `shed workspace new` runs in (`os.Getwd()`). Set it only when the agent may have `cd`'d away first. |

Why env vars over a flag: an in-session model won't invent an env var it was
never told about, whereas it *will* try to fill a flag it sees in `--help`.
Inheritance also means the orchestrator sets it once and every `shed workspace
new` in that process is covered, with no per-call cooperation from the agent.
(If a flag is ever wanted for symmetry, it must be `MarkHidden` so it stays out
of `--help`.)

## Open questions / must-verify

1. **Empirically confirm** the hook-visible id == the resume id for Cursor
   (`conversation_id`) and opencode (`input.sessionID`). Claude is documented.
2. **opencode `output.args.command` field name** on the installed version
   (documented for the built-in bash tool; confirm it hasn't drifted).
3. **Claude cwd semantics** — *resolved: it does drift.* The `PreToolUse` `cwd`
   is the agent's transient working directory, which moves as the model `cd`s
   mid-session, so it is **not** the session's project dir and `claude --resume`
   from it fails ("No conversation found"). The launch dir is instead derived
   from the `PreToolUse` `transcript_path` (also delivered in the same event):
   the transcript lives under `~/.claude/projects/<munge(launch-cwd)>/` and its
   first entry records that launch cwd verbatim, so the hook reads it from there
   (`launchCWDFromTranscript`) and links that. Falls back to the hook `cwd` if
   the transcript can't be read. opencode/Cursor have no `transcript_path` and
   keep the hook cwd — acceptable for Cursor (resume isn't cwd-scoped); opencode
   is still open (see note below).
4. **Link lifecycle (safety-critical).** A link is subordinate metadata; its
   staleness must **never** delete a workspace. The only things that remove a
   workspace stay exactly as today: the `shed prune` landed-work rule (merged
   PR / merged into default) and explicit `shed workspace rm`. A dead or missing
   session is **not** a deletion trigger — losing resumability is a minor,
   recoverable degradation; losing a workspace (possibly holding unpushed work)
   is not.
   - A link rides along with its workspace: removed only when the workspace is
     removed. No standalone link-reaping job.
   - A dangling link is handled **lazily at resume**, and only partially:
     `shed resume <name>` verifies the session's launch *directory* still
     exists and reports "the session's directory `<cwd>` no longer exists;
     cannot resume (workspace is intact at `<path>`)". A missing or expired
     *transcript* with an intact directory is not detected — the agent's own
     error surfaces in that case — and the dead link is left in place; resume
     never touches the workspace itself.

## Implementation sequence

1. Rename the workspace-layer "branch" concept to *workspace name* (it's
   already the directory name, not the live git branch) — including the
   `shed ls` / `workspace ls` column header (BRANCH → NAME). Enforce unique
   workspace names in `shed workspace new` (reject a `<name>` already present
   under any repo, naming the conflict). Add a `workspace.LocateByName(name)`
   lookup used by both the guard and resume.
2. Pre-exec hook subcommand (`shed __on-tool-call`) that parses the `<name>`
   and writes the `pending/<name>` intent record.
3. `shed workspace new` finalize step: write the `.git/shed.session` sidecar
   from the `SHED_SESSION_*` env override (no visible flag) or the pending
   intent, then clear the pending record.
4. `shed resume <name>` command (single-name enforcement done by hand via
   `ArgsLenAtDash`, per §5, so the `--` passthrough parses; + exec/`--print`).
   Optionally annotate `shed ls` workspaces with their linked session/agent.
5. `shed init` wiring: install the pre-exec hook for each agent; extend the
   opencode plugin.
6. Update the embedded guide so agents know to pick distinct workspace names and
   that `shed resume` exists. *(Not yet done — the guide mentions neither.)*
