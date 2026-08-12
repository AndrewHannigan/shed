# shed

![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go) ![Status](https://img.shields.io/badge/status-beta-yellow) ![License](https://img.shields.io/badge/license-MIT-green)

**git repo management for terminal coding agents**

![shed in action](docs/demo.gif)

<sub>[▶ interactive version](https://asciinema.org/a/r3yPwD6Iz6RqMk7S?speed=2) — pause, scrub, copy text</sub>

Shed keeps a catalog of read-only, always-current repo checkouts under `~/.shed/repos`, and hands agents cheap, disposable workspaces when they need to write. The catalog is read-only at the OS level and synced at the start of every agent session, so agents read and branch from fresh code and can't corrupt the reference copies. One Go binary, no daemon, no telemetry.

```text
You:   "Fix the broken link in octocat/Hello-World's README"
Agent: reads ~/.shed/repos/github.com/octocat/Hello-World   (read-only, current)
       → shed workspace new                                 (writable clone off the latest)
       → edits there, opens a PR                            (catalog and other agents untouched)
```

Agents run all of this themselves. `shed init` wires up the integration once; after that you stop managing repos and worktrees on your agents' behalf.

- Works the same across Claude Code, Cursor CLI, and opencode. Parallel sessions in the same repo don't collide - each task gets its own workspace.
- Repos sync in the background at session start and again right before each workspace is created, so an agent never branches off stale code by accident.
- `shed workspace new` is purely local (objects hardlink from a shared per-upstream mirror), so creation takes seconds and works offline. The result is an ordinary git repo with `origin` pointing at the real upstream; push as if you'd cloned it from there.
- `shed prune` deletes workspaces whose work already landed (merged PR, or merged into the default branch), refuses dirty or unpushed work.
- Track several versions of one repo: `cpython`, `cpython@3.12`, and `cpython@v3.12.3` sit side by side and share one mirror.

## Install

```bash
# macOS (Homebrew)
brew trust --tap AndrewHannigan/tap && brew install AndrewHannigan/tap/shed

# Linux / other Unix
curl -fsSL https://raw.githubusercontent.com/AndrewHannigan/shed/main/install.sh | sh
```

The script installs a release binary and verifies its checksum against the GitHub release; binaries are also on [Releases](https://github.com/AndrewHannigan/shed/releases) if you'd rather skip `curl | sh`. Linux and macOS; on Windows, use WSL.

## Quickstart

```bash
# integrate with your agents
shed init

# add a repo (github shorthand works), or a whole owner
shed add octocat/Hello-World
shed add python

# optionally pin extra versions — a branch that follows, or a tag frozen in time
shed add python/cpython --track 3.12

# now run claude, cursor-agent, or opencode — your agent knows how to use it
```

Division of labor: you (or your agent) curate the catalog with `shed add` / `shed rm`. The `workspace` commands are best left to the agent — it creates a workspace the moment it needs to make a change and tears it down when done.

When PRs are merged, `shed prune` reclaims the workspaces they leave behind.

## Why plain clones for workspaces, not `git worktree`

Repos *are* worktrees — of the mirror, which is what makes N versions of a repo cost one copy of history. Workspaces deliberately are not.

Worktrees impose a workflow: all trees of a repo pool one `refs/heads/*` namespace (and by default one `.git/config`), and git polices the sharing — you can't check out or delete a branch another worktree holds. That's fine for a single careful human; it's coordination overhead for agents working in parallel. A clone owns its refs, reflogs, config, index, and `HEAD` outright. An agent can branch freely, rewrite history, retarget `origin`, or change `user.email`, and nothing it does touches another workspace. Teardown is `rm -rf` — no registration bookkeeping.

Those arguments apply only to the writable tier. The read-only repos never branch, never push, and have a single owner (shed), so they get worktrees; workspaces get clones.

## Commands

| Command | What it does |
|---------|--------------|
| `shed init` | Bootstrap dirs + integrate with detected agents |
| `shed add <repo\|owner>` | Add a repo — or a whole user/org — to the catalog |
| `shed rm <name>…` | Remove tracked repos, owners, or a single workspace |
| `shed ls` | List owners, repos, and workspaces — everything shed manages |
| `shed repo ls\|add\|rm` | Same operations, scoped to repos only |
| `shed owner ls\|add\|rm` | Same operations, scoped to tracked users/orgs |
| `shed sync [<name>…]` | Fetch mirrors and refresh the read-only repos (usually automatic) |
| `shed status` | Report sync health; show a repo's error and the likely fix |
| `shed workspace new <repo> <branch>` | Create a writable clone off the freshly-synced repo; prints its path |
| `shed workspace ls` | List workspaces with dirty/unpushed state and age |
| `shed workspace rm <name>…` | Delete workspaces (refuses dirty/unpushed work without `--force`) |
| `shed path <name>` | Print the absolute path of a repo or workspace (for `cd "$(shed path <name>)"`) |
| `shed prune` | Delete workspaces whose work has landed; run mirror gc and orphan cleanup |
| `shed history` | Show recent shed commands |
| `shed help [topic]` | Long-form docs on a command or concept |

## Supported agents

`shed init` auto-detects each agent by config-dir presence and (in TTY mode) prompts before writing anything:

| Agent | Config dir | Allowed-dir setting | SessionStart hooks |
|-------|-----------|---------------------|--------------------|
| Claude Code | `~/.claude/` | `settings.json` → `permissions.additionalDirectories` | session-context + bg-sync |
| Cursor CLI | `~/.cursor/` | n/a² | session-context + bg-sync (`hooks.json`)² |
| opencode | `~/.config/opencode/` | n/a¹ | plugin (see below)¹ |

¹ opencode has no SessionStart shell hook and no path allowlist. Instead, `init` drops a plugin at `~/.config/opencode/plugin/shed.js`, auto-loaded at startup; it runs `shed __bg-sync` and injects the guide into the model's system prompt via opencode's `experimental.chat.system.transform` hook.

² Cursor's hooks live in `~/.cursor/hooks.json` under `hooks.sessionStart`. shed adds two entries — `shed __session-context --agent cursor` (prints an `{"additional_context":"…"}` object that Cursor injects into the conversation) and `shed __bg-sync`. Cursor has no per-directory allowlist; the `chmod a-w` on `repos/` enforces read-only.

Why a SessionStart hook instead of a static doc or a skill: skills load lazily, so the agent sees them only after something triggers, but it needs to reach for shed *before* doing the wrong thing (cloning into `/tmp`, hallucinating a path). The hook injects the guide into context at the start of every session, and because the text is generated by the binary rather than read from a file, it can't drift after an upgrade.

## Uninstall

```bash
shed init --uninstall           # reverse the agent integration, keep repos and workspaces
shed init --uninstall --purge   # also delete ~/.shed and ~/.config/shed entirely
```

All edits `shed init` makes are idempotent and recorded in a sidecar state file, so `--uninstall` removes exactly what shed added and nothing else.

## Layout on disk

```
~/.config/shed/
└── config.toml                                # your tracked repos

~/.shed/
├── repos/<host>/<owner>/<repo>[@<track>]/     # read-only checkouts (chmod a-w)
├── workspaces/<host>/<owner>/<repo>[@<track>]/<name>/   # editable clones
├── logs/
└── .internal/                                 # plumbing — mirrors, locks, records
    └── mirrors/<host>/<owner>/<repo>/         # one fetch-only mirror per upstream
```

Each repo is a worktree of its upstream's mirror, so tracking several versions of a big repo costs one copy of its history, and syncing them costs one network fetch.

Config example:

```toml
[[repo]]
url = "https://github.com/octocat/Hello-World"

[[repo]]
url = "git@github.com:foo/bar.git"
name = "myorg/bar"   # optional override; default derived from URL (and track)

# Pin a branch or tag. A branch advances on every sync; a tag never changes.
# These live at cpython@3.12 and cpython@v3.12.3 next to a default-branch
# cpython, all sharing one mirror.
[[repo]]
url = "https://github.com/python/cpython"
track = "3.12"

[[repo]]
url = "https://github.com/python/cpython"
track = "v3.12.3"

# Per-repo git config. Reconciled into the cache on every sync and seeded into
# new workspaces at clone time — forwarded verbatim, so any git option works.
[[repo]]
url = "https://github.com/myorg/widgets"
git = { "user.email" = "me@work.com", "core.hooksPath" = ".githooks" }

# Track a whole user/org. sync discovers its repos via gh and adds new ones
# automatically.
[[owner]]
url = "https://github.com/octocat"
# include_forks = false      # default
# include_archived = false   # default
# visibility = "all"         # all|public|private
```

## Authentication

Shed does not manage credentials. Every git operation defers to whatever `git clone <url>` already works with in your shell — HTTPS credential helpers or `ssh-agent`. If `git clone <url>` works, shed works.

GitHub shorthand (`shed add owner/repo`) expands to HTTPS. If HTTPS can't authenticate but SSH can — the common "I only have an `ssh-agent` set up" case — `shed add` detects this during a preflight check and stores the working SSH URL instead, telling you it did. To force a transport, pass a full URL.

Sync failures — including a repo's very first clone — are recorded and surfaced, never silently dropped: `shed status` reports them with a transport-aware fix, and the session-start hook warns your agent that the store is stale.

## Documentation

- `shed help` — curated overview of every command
- `shed help <topic>` — long-form prose docs on a command or concept (topics: `agents`, `auth`, `concepts`, `history`, `init`, `catalog`, `locking`, `owner`, `path`, `prune`, `sync`, `workspace`)
- `shed --help` and `shed <cmd> --help` — flag reference

## License

[MIT](./LICENSE) — © 2026 Andrew Hannigan.
