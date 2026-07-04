# shed

![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go) ![Status](https://img.shields.io/badge/status-beta-yellow) ![License](https://img.shields.io/badge/license-MIT-green)

**git repo management for terminal coding agents.**

**shed** is one standard system all your agents share to locally manage git repos and workspaces — read-only reference repos, isolated writable workspaces, and improved session resumption.

<!-- TODO(visual hook): drop a demo GIF/asciinema here — two agents working the same
     repo through shed: each gets its own fresh `shed workspace new`, neither touches the
     read-only store, then `shed prune` cleans up. This is the "10-second, read-no-text"
     hook; keep it above the fold. -->
<!-- ![shed in action](docs/demo.gif) -->

- 🤝 **One system, every agent** — All agents manage repos and workspaces the same way, so parallel sessions never step on each other in the same repo.
- ✍️ **Isolated writable workspaces** — `shed workspace new` gives each session its own clone off the pristine repo; agents edit there, never in your reference copy or each other's. Creation is purely local (objects hardlink from a shared mirror), so it's fast and works offline.
- 🌱 **Never a stale branch** — every workspace is created from the freshly-synced repo, so an agent never unintentionally works on out-of-date code.
- 🗂 **Multiple versions, one repo** — track a branch or tag with `shed add <repo> --track <ref>`; `airflow`, `airflow@v2-7-stable`, and `airflow@2.7.3` sit side by side, sharing one mirror so extra versions cost a checkout, not another copy of history.
- 🧹 **One-command cleanup** — workspaces pile up fast; `shed prune` reclaims the ones whose work has already landed (merged PR or merged into the default branch) and leaves anything unpushed untouched.
- 🔁 **Pick up where you left off** — `shed resume <workspace>` reopens the exact agent session that created a workspace — same agent, same session id, same directory — so a half-finished task is one command away.
- 🧰 **Searchable out of the box** — agents run `rg`, `grep`, `git`, and `gh` across the entire catalog directly.
- ⚙️ **Zero agent setup** — one `shed init` wires up each agent to use shed automatically.

---

## Install

```bash
# macOS (Homebrew)
brew install AndrewHannigan/tap/shed

# Linux / other Unix
curl -fsSL https://raw.githubusercontent.com/AndrewHannigan/shed/main/install.sh | sh
```

---

## Quickstart

```bash
# integrate with your agents
shed init

# add a repo (github shorthand works)
shed add octocat/Hello-World

# now run claude, cursor-agent, or opencode — your agent knows how to use it
```

That's it. Now any of your agents have a consistent system for working with your repo catalog — reading the read-only copy, and carving off an isolated, up-to-date workspace the moment they need to make changes:

```text
You:   "Fix the broken link in octocat/Hello-World's README"
Agent: reads ~/.shed/repos/github.com/octocat/Hello-World   (read-only, always fresh)
       → shed workspace new                                 (isolated, off the latest)
       → edits there, opens a PR                            (store + other agents untouched)
```

Once branches land, reclaim the workspaces they left behind:

```bash
shed prune          # remove workspaces whose work is already merged
```

Need to get back into one? `shed resume <workspace>` relaunches the agent session that
created it — in its original working directory — so you can continue the task instead of
re-explaining it.

> **Who runs what.** `shed add` / `shed rm` curate the library — run them yourself,
> or let your agent run them when it needs a repo. The `shed workspace` commands are
> best left to the agent: it creates a workspace the moment it needs to make a change
> and tears it down when done. You generally don't pre-create workspaces — a stale,
> hand-made one just risks the agent branching off the wrong base, which is exactly
> what shed exists to avoid. Set up the library; let the agent manage its own scratch space.

---

## Commands

| Command | What it does |
|---------|--------------|
| `shed init` | Bootstrap dirs + integrate with detected agents (`--uninstall` reverses it) |
| `shed add <repo\|owner>` | Add a repo — or a whole user/org — to the library |
| `shed rm <name>…` | Remove tracked repos or owners (and their stores/workspaces) |
| `shed ls` | List owners, repos, and workspaces — everything shed manages |
| `shed repo ls` | List just the read-only repos (no owners or workspaces) |
| `shed repo add <repo\|owner>` | Same as `shed add` (grouped under the `repo` noun) |
| `shed repo rm <name>…` | Same as `shed rm` (grouped under the `repo` noun) |
| `shed owner ls` | List just the tracked users/orgs and their repo counts |
| `shed owner add <owner>` | Track a user/org (forces the owner reading, even for `owner/repo`) |
| `shed owner rm <name>…` | Drop one or more tracked owners (resolves against owners only) |
| `shed sync [<name>…]` | Fetch each upstream's mirror once and refresh the read-only repos (usually automatic) |
| `shed status` | Report sync health; show a repo's error and the likely fix |
| `shed workspace new <repo> <branch>` | Create a writable clone off the freshly-synced repo (purely local); prints its path |
| `shed workspace ls` | List workspaces with dirty/unpushed state and age |
| `shed workspace rm <name>…` | Delete one or more workspaces (refuses dirty/unpushed work without `--force`) |
| `shed path <name>` | Print the absolute path of a repo or workspace by name (for `cd "$(shed path <name>)"`) |
| `shed prune` | Delete workspaces whose work has already landed; run shed's maintenance (mirror gc, orphan cleanup) |
| `shed resume <name>` | Reopen the agent session that created a workspace |
| `shed history` | Show recent shed commands |
| `shed help [topic]` | Long-form docs on a command or concept |

Curate the library yourself (`add`/`rm`/`ls`); leave the `workspace` commands to the agent.

---

## Supported agents

`shed init` auto-detects each agent by config-dir presence and (in TTY mode) prompts before writing anything:

| Agent | Config dir | Allowed-dir setting | SessionStart hooks |
|-------|-----------|---------------------|--------------------|
| Claude Code | `~/.claude/` | `settings.json` → `permissions.additionalDirectories` | session-context + bg-sync |
| Cursor CLI | `~/.cursor/` | n/a² | session-context + bg-sync (`hooks.json`)² |
| opencode | `~/.config/opencode/` | n/a¹ | plugin (see below)¹ |

¹ opencode has no SessionStart shell hook and no path allowlist. Instead, `init` drops a plugin at `~/.config/opencode/plugin/shed.js`, auto-loaded at startup; it runs `shed __bg-sync` and injects the guide into the model's system prompt via opencode's `experimental.chat.system.transform` hook. `shed init --uninstall` deletes the file.

² Cursor's hooks live in `~/.cursor/hooks.json` under `hooks.sessionStart` (a flatter, camelCase shape than Claude's). shed adds two `sessionStart` entries — `shed __session-context --agent cursor` and `shed __bg-sync`. The session-context one prints a `{"additional_context":"…"}` JSON object that Cursor injects into the conversation. Cursor has no per-directory allowlist (like opencode, the `chmod a-w` on `repos/` enforces read-only), so no paths are registered. If a hand-rolled `~/.cursor/plugins/local/shed` plugin is present, `init` removes it so the guide isn't injected twice.

All edits are idempotent and recorded in a sidecar state file, so `shed init --uninstall` removes only what shed added.

**Why a SessionStart hook and not a static doc or a skill?** Skills load lazily — the agent only sees them once something triggers, but the whole point is that the agent reaches for shed *before* doing the wrong thing (cloning into `/tmp`, hallucinating a path). The `shed __session-context` hook injects the guide into context at the start of every session. Because that text is generated by the binary rather than written to a file, it's always current — there's nothing to drift after an upgrade.

---

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

Everything shed prints as a destination is top-level; everything else lives under
`.internal/`. Each repo is a worktree of its upstream's mirror, so tracking several
versions of a big repo costs one copy of its history, and syncing them costs one
network fetch.

Config example:

```toml
[[repo]]
url = "https://github.com/octocat/Hello-World"

[[repo]]
url = "git@github.com:foo/bar.git"
name = "myorg/bar"   # optional override; default derived from URL (and track)

# Pin a branch or tag. A branch advances on every sync; a tag never changes.
# Names gain an @<track> suffix: these live at airflow@v2-7-stable and
# airflow@2.7.3 next to a default-branch airflow, all sharing one mirror.
[[repo]]
url = "https://github.com/apache/airflow"
track = "v2-7-stable"

[[repo]]
url = "https://github.com/apache/airflow"
track = "2.7.3"

# Per-repo git config. Reconciled into the cache on every sync and seeded into
# new workspaces at clone time — forwarded verbatim, so any git option works.
[[repo]]
url = "https://github.com/myorg/widgets"
git = { "user.email" = "me@work.com", "core.hooksPath" = ".githooks" }

# Track a whole user/org. sync discovers its repos via gh and adds new ones
# automatically (as [[repo]] entries tagged with source = this owner).
[[owner]]
url = "https://github.com/octocat"
# include_forks = false      # default
# include_archived = false   # default
# visibility = "all"         # all|public|private
```

---

## Why read-only repos + writable workspaces

The natural first instinct is "just keep a normal clone of each repo and let agents work in it." That breaks down the moment you have more than one thing going on:

- **One clone has one working tree and one `HEAD`.** Two agents — or one agent on two tasks — can't both use it. One has to stash, switch branches, and pray; the other clobbers it. Splitting a *read-only reference* from *N disposable workspaces* gives every task its own tree and refs, so they run in parallel without colliding.
- **The reference stays trustworthy.** Because the repo checkout is `chmod a-w`, it's never half-edited, never parked on some branch an agent forgot to leave, never carrying stray uncommitted changes. So searching and reading across the catalog always reflects real upstream code, and every new workspace forks from a known-good, current copy — never a stale branch by accident.
- **Mistakes are cheap.** An agent literally can't corrupt the source of truth. Workspaces are throwaway: if one goes sideways, delete it (or `shed prune`) and the pristine copy is untouched.

So read-only isn't the goal in itself — it's what makes the *writable* workspaces safe to hand out freely. You get a stable baseline to read from **and** isolated, always-fresh scratch space to write in, instead of having to trade one for the other.

## How it's built: one mirror per upstream, three tiers

Behind the two things you see (repos and workspaces) sits one piece of plumbing (the mirror), with a single invariant dividing them: **only shed writes to mirrors and repos; agents only ever write to workspaces.**

| Tier | What it is | Writable by | Lifetime |
|---|---|---|---|
| **mirror** (`.internal/mirrors/…`) | fetch-only repo, tree never checked out | shed (network fetch) | permanent, one per upstream |
| **repo** (`repos/…`) | worktree of the mirror on its tracked branch (or detached at a tag) | shed (fast-forward on sync) | permanent, N per mirror |
| **workspace** (`workspaces/…`) | plain local clone off a repo, origin → the real upstream | agents | disposable |

Why this shape:

- **Fetches can't be blocked.** The mirror fetches upstream truth into `refs/remotes/origin/*`, a namespace no checkout can occupy — so nothing an agent does inside a repo checkout can ever fail the shared fetch. Damage is contained to that one checkout, which repairs itself on the next sync.
- **Repos sit on real branches.** `git status` in a repo says `On branch v2-7-stable`, not a detached hash; a tag checkout reads `HEAD detached at 2.7.3`. Sync is a fast-forward merge; a force-pushed upstream is detected and reported, then reset.
- **Workspaces stay 100 % ordinary.** A workspace is `git clone <repo> && git remote set-url origin <upstream>` — objects hardlink from the mirror, so creation is local-disk fast and works offline, and the result behaves exactly like a clone of GitHub. Git's own auto-gc keeps long-lived workspaces healthy; shed never touches them in the background.
- **Nothing shed does in the background can lose agent work.** Sync writes only to shed-owned tiers; a mirror `gc` can never prune what a repo or hardlinked workspace needs (every checkout is a reachability root); cleanup of finished workspaces happens only in explicit `shed prune`, which refuses dirty or unpushed work without `--force`.
- **Maintenance is shed's job.** You never run `git gc`: `shed prune` compacts each mirror, sweeps stale bookkeeping, and deletes mirrors no config entry references — at a moment you can see, never behind your back.

## Why plain clones for workspaces, not `git worktree`

Repos *are* worktrees (of the mirror — that's what makes N versions cheap and keeps them dependency-tracked through gc). Workspaces are deliberately not:

- **Each workspace is just an ordinary repo, with no shared namespace to coordinate.** Worktrees pool one `refs/heads/*` (and the branch reflogs under it), and by default one `.git/config`, across every tree — so git has to police the sharing: you can't check out or delete a branch another worktree holds. A clone owns its refs, branch reflogs, config, index, `HEAD`, and an `origin` pointing at the real upstream. An agent can branch, delete, retarget `origin`, change `user.email`, or rewrite history, and none of it touches another workspace or needs any special setup.
- **Teardown is a plain `rm -rf`** — all `shed prune` and `workspace rm` do. The worktree equivalent is `git worktree remove`, with registration bookkeeping to keep straight.
- **A plain clone leaves room — for the agent and for shed.** Because a workspace is an ordinary git repo with no worktree rules bolted on, an agent can drive it however a task demands, and pushing to the real upstream needs no ceremony.

Those arguments are exactly why the *user-facing writable* tier is clones — and none of them apply to the read-only repos, which never push, never branch, and are maintained by one owner (shed). That's why repos get worktrees and workspaces get clones.

---

## Authentication

Shed does not manage credentials. Every git operation defers to whatever `git clone <url>` already works with in your shell — HTTPS credential helpers or `ssh-agent`. If `git clone <url>` works, shed works.

**Picking a transport.** GitHub shorthand (`shed add owner/repo`) expands to HTTPS. If that can't authenticate but the SSH form can — the common "I only have an `ssh-agent` set up" case — `shed add` detects this during a preflight check and stores the working SSH URL instead, telling you it did. To force a transport, pass a full URL (`git@github.com:owner/repo.git` or `https://github.com/owner/repo`).

**When auth fails.** Sync failures — including a repo's very first clone — are recorded and surfaced, never silently dropped: `shed status` reports them and the session-start hook warns your agent that the store is stale. The suggested fix is transport-aware (load your SSH key vs. `gh auth login` / a credential helper).

---

## Documentation

- `shed help` — curated overview of every command
- `shed help <topic>` — long-form prose docs on a command or concept (topics: `agents`, `auth`, `concepts`, `history`, `init`, `library`, `locking`, `owner`, `prune`, `sync`, `workspace`)
- `shed --help` and `shed <cmd> --help` — flag reference

## License

[MIT](./LICENSE) — © 2026 Andrew Hannigan.
