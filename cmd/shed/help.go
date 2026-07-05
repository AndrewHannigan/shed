package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/errs"
)

// overviewTopline is the first line of the curated overview, shared by
// `shed help`, bare `shed`, and `shed --help`.
const overviewTopline = "shed — git repo management for terminal coding agents"

func newHelpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "help [topic]",
		Short: "Long-form docs on a command or concept",
		Long: `help prints prose documentation for a command or concept.
With no topic, prints an overview.

Topics: ` + strings.Join(topicList(), ", "),
		Args:               cobra.MaximumNArgs(1),
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			topic := "overview"
			if len(args) == 1 {
				topic = args[0]
			}
			return runHelp(topic)
		},
	}
}

func runHelp(topic string) error {
	if alias, ok := helpAliases[topic]; ok {
		topic = alias
	}
	text, ok := helpTopics[topic]
	if !ok {
		return errs.New(errs.NotFound, "unknown topic %q (try: %s)",
			topic, strings.Join(topicList(), ", "))
	}
	fmt.Print(text)
	return nil
}

// helpAliases lets `shed help <command>` resolve to the topic that documents
// it, even when several commands share one topic (e.g. add/rm/ls → library).
// Without these, `shed help add` would error despite `add` being a real command.
var helpAliases = map[string]string{
	"add":       "library",
	"rm":        "library",
	"ls":        "library",
	"repo":      "library",
	"new":       "workspace",
	"from-pr":   "workspace",
	"uninstall": "init",
	"purge":     "init",
}

func topicList() []string {
	keys := make([]string, 0, len(helpTopics))
	for k := range helpTopics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var helpTopics = map[string]string{

	"overview": `shed — git repo management for terminal coding agents

Manages a read-only store of your git repos and hands your agents isolated,
writable workspaces to make changes. Run 'shed init' to begin.

Getting started:

    # One-time init to teach your agents how to use shed
    shed init

    # Add a repo or a user/org to the library (GitHub short-form allowed)
    shed add AndrewHannigan/shed       # Add a repo
    shed add octocat                   # Add an owner, auto-sync future repos

    # Your agents now know how to use shed. Ask for a tour.

Supports claude, cursor-agent, and opencode.

Commands:
  add           add a repo (or a whole user/org) to the library
  help <topic>  long-form docs (also accepts a command name, e.g. 'shed help add')
  history       show recent shed commands
  init          bootstrap + integrate with detected agents (--uninstall reverses it)
  ls            list owners, repos, and workspaces (everything shed manages)
  owner         {ls,add,rm} of tracked users/orgs
  path          print the absolute path of a repo or workspace by name
  prune         delete workspaces whose work has already landed
  repo          {ls,add,rm} of the read-only repo library
  resume        reopen the agent session that created a workspace
  rm            remove tracked repos or owners
  status        report sync health; show a repo's error and the likely fix
  sync          fetch each upstream once, refresh the read-only repos (usually automatic)
  workspace     {new,from-pr,ls,rm} of writable workspaces

Topics: agents, auth, concepts, history, init, library, locking, owner, path, prune, sync, workspace
`,

	"concepts": `Concepts

You only ever think about two things: repos you read and workspaces
agents write in. The machinery behind them stays invisible unless you go
looking.

Library
  The set of repos you've told shed to track. Stored in
  ~/.config/shed/config.toml. Edit via 'shed add/rm/ls'.

Repo
  A permanent, browsable, read-only checkout of one library entry, kept
  with its working tree marked read-only (chmod -R a-w). Lives under
  ~/.shed/repos/<host>/<owner>/<repo>[@<track>]/. A repo follows its
  configured 'track': the upstream default branch when unset, or a pinned
  branch (advances on every sync) or tag (never changes). Several versions
  of one upstream can sit side by side (cpython, cpython@3.12,
  cpython@v3.12.3), each independently referenceable.

Mirror (plumbing)
  Behind the repos, shed keeps one fetch-only mirror per upstream under
  ~/.shed/.internal/mirrors/. Every version of a repo is a worktree of
  its mirror, so N versions cost one copy of history and one network
  fetch per sync. Mirrors are created on demand, never configured, and
  only surface in sync output and 'shed prune' (which does their upkeep).

Workspace
  An editable, completely ordinary git clone made from a repo's checkout;
  its origin points at the real upstream, so committing and pushing work
  like any clone. Identified by (repo, name). Creation is purely local —
  objects hardlink from the mirror — so it is fast and works offline.
  Lives under ~/.shed/workspaces/<host>/<owner>/<repo>[@<track>]/<name>/.

Agent integration
  Per-agent edits that 'shed init' makes so each terminal agent:
  (a) knows shed exists (via a SessionStart hook that injects the
  shed guide into the session context), (b) has filesystem access
  to the repos and workspaces directories, (c) refreshes the repos in
  the background at session start.
`,

	"init": `init — bootstrap + agent integration (--uninstall reverses it)

Creates the shed config and data directories if missing, writes an
empty config file, and in TTY mode prompts to install integration for
each detected agent.

Flags:
  --agents=auto|all|none|<list>   which agents to integrate (default auto)
  --no-bg-sync                    skip the SessionStart bg-sync hook
                                  (keeps the session-context hook)
  --uninstall                     reverse agent integration instead of
                                  installing it (see below)
  --purge                         with --uninstall, also delete the data
                                  and config dirs (see below)

Modes:
  auto (TTY)    detect installed agents and prompt to install
  auto (non-TTY) skip agent integration silently (scripted use)
  all           install for every supported agent, even undetected ones
  none          skip agent integration entirely
  <list>        comma-separated agent keys, e.g. claude,cursor

Re-running init is safe: directory and hook entries are idempotent. The
guide the agent sees is generated by the binary at session start (see
'shed __session-context'), so it stays current across upgrades with
no re-init needed.

Reversing integration ('shed init --uninstall')
  Removes exactly the entries 'shed init' added to each agent's config:
    - the allowed-directory entries in Claude's settings.json
    - the SessionStart hooks (session-context and bg-sync) / the
      opencode plugin file
  A sidecar state file (~/.shed/agents.state.json) records which entries
  are shed's, so any entries you added yourself are preserved. This does
  NOT remove the shed binary, and by default leaves ~/.config/shed/ and
  ~/.shed/ in place.

  Add --purge to also delete both directories, removing all stored repos,
  workspaces, and config. If any workspace has uncommitted or unpushed
  work, --purge lists those workspaces and asks for confirmation before
  deleting (and refuses when stdin is not a TTY).
`,

	"library": `library — manage tracked repos and owners

  shed add <repo> [--track <ref>] [--name <n>] [--owner|--repo]
    Add a repo to the library. <repo> may be a full git URL or GitHub
    shorthand: a bare 'owner/repo' or 'owner' is expanded against
    github.com, so 'shed add octocat/Hello-World' works.
    Name defaults to <host>/<owner>/<repo> derived from the URL. --name
    overrides. Fetches the new repo right away (runs a scoped 'sync').
    Exit 3 if the name already exists.

    --track <ref> pins the checkout to a branch or tag instead of the
    upstream default branch: a branch advances on every sync, a tag never
    changes. The name gains an '@<track>' suffix (cpython@3.12), and
    several versions of one repo can be tracked side by side — they share
    one mirror on disk, so an extra version costs a checkout, not another
    copy of history. Bare names prefer a branch over a same-named tag; pin
    with 'heads/<n>' or 'tags/<n>'. Changing a repo's track is an identity
    change: remove and re-add. One entry per (url, track) is enforced.

    If <repo> is a bare user/org (one path segment, e.g. octocat or
    https://github.com/octocat) it is tracked as an owner instead;
    sync then discovers and adds that owner's repos. Detection is automatic;
    --owner / --repo force it. See 'shed help owner'.

  shed rm <name>... [--force]
    Remove a repo completely: the config entry, the store on disk, and
    every workspace derived from it. When the removal would also delete a
    workspace, rm asks for confirmation first. Restores write permissions
    on the read-only store tree automatically.

    If <name> is an owner, removes the owner entry and every repo it
    auto-added, along with their workspaces and stores — again asking for
    confirmation first. Answering no keeps the repos: they stay on disk,
    just untied from the owner (so a later sync no longer manages them).

    Several names may be given at once ('shed rm a b c'); each is removed
    independently, so a failure on one is reported but doesn't stop the rest.

    --force skips the confirmation prompt and discards uncommitted or
    unpushed work without asking. When stdin is not a TTY, rm will not
    delete workspaces without --force: a repo removal refuses, and an
    owner is untied instead.

  shed ls [--json]
    Show everything shed manages, in three captioned sections so it's
    clear what each is:
      Owners      whole users/orgs you track; sync auto-adds their repos
      Repos       read-only reference copies, with last sync and (when an
                  owner auto-added it) which owner did
      Workspaces  isolated writable clones, with dirty/unpushed state and age
    The Owners and Workspaces sections are omitted when empty (a hint to
    create your first workspace is shown when you have repos but none yet).

These are also grouped under a 'repo' noun (mirroring 'workspace'):
'shed repo add' and 'shed repo rm' are the same as 'shed add'/'shed rm',
and 'shed repo ls' lists just the repos — where plain 'shed ls' also
includes the Owners and Workspaces sections.
`,

	"owner": `owner — track a whole user or org

Add an owner with 'shed add <owner-url>' (a URL with a single
path segment, e.g. https://github.com/octocat). On every sync,
shed lists that owner's repos and adds any new ones to the library
automatically, so repos created upstream after you start tracking are
picked up and fetched without another 'add'. This also happens in
the background at each agent session start (see 'shed help sync').

Discovery uses the 'gh' CLI — shed's only dependency beyond 'git',
and only for discovery. Once a repo has been discovered it is an ordinary
library entry that syncs with plain 'git'. So if 'gh' is missing or not
authenticated, shed degrades gracefully: it warns and skips
discovery, but already-known repos still sync.

By default an owner pulls its non-fork, non-archived repos (including
private ones you can access). Tune per owner in config.toml:

  [[owner]]
  url = "https://github.com/octocat"
  include_forks = false       # default
  include_archived = false    # default
  visibility = "all"          # all|public|private

Reconciliation is additive: repos that disappear upstream are left in
place (so a workspace with unpushed work is never deleted out from under
you). Remove them yourself with 'shed rm <name>', or drop the
whole owner with 'shed rm <owner>'.

Owners also have their own noun (mirroring 'repo' and 'workspace'):

  shed owner ls [--json]   list just the tracked owners and their repo counts
  shed owner add <owner>   track an owner (forces the owner reading, so even
                           an 'owner/repo' argument is tracked as an owner)
  shed owner rm <name>...  drop one or more owners; names resolve against
                           owners only (a repo name here is "not in the config")

These are the owner-scoped forms of 'shed add'/'shed rm'/'shed ls': use them
when you mean an owner specifically. 'shed owner add' is 'shed add --owner',
and 'shed owner rm' is 'shed rm' restricted to owners.
`,

	"sync": `sync — fetch each upstream's mirror, refresh the repos

  shed sync [<name>...] [--if-older-than <dur>] [--jobs N] [--json]

Before fetching, sync expands any tracked owners in scope: it lists each
owner's repos via 'gh' and adds new ones to the library, then fetches them
in the same pass (a brand-new upstream has no mirror, so it is cloned).
Naming an owner syncs all of its repos. If 'gh' is unavailable, discovery
is skipped with a warning and already-known repos still sync. See
'shed help owner'.

A repo whose remote no longer resolves on fetch (deleted, renamed, or
access revoked) is reported as "gone upstream" rather than a failure: it is
counted apart from failures, does not affect the exit code, and is left in
place with a hint to remove it with 'shed rm'. sync never deletes a repo or
its workspace on your behalf.

A failed fetch (offline, auth, upstream down) does not block the local
phase: checkouts are still created or kept from the mirror's last-synced
state, so adding a new version of an already-mirrored repo materializes
even offline — every branch and tag from the last fetch is already on
disk. A tag-pinned repo then counts as ok (a tag never changes, so its
checkout is exact, not stale); a branch-tracked repo reports the failure
and stays flagged until a fetch succeeds, since its checkout may be
behind. Only a repo whose mirror has never been cloned at all hard-fails
offline.

Behavior, in two phases per upstream mirror:

  network (one fetch per upstream, however many versions you track):
  1. Create the mirror if missing (fetch-only, tree never checked out;
     built in a temp dir and renamed into place, serialized by a creation
     lock beside the mirror so concurrent first syncs can't collide).
  — under the mirror's exclusive lock —
  2. If --if-older-than D and the last fetch is fresher than D, skip.
  3. git fetch --prune --prune-tags (upstream truth lands in
     refs/remotes/origin/*, which no checkout can block).
  4. Refresh the recorded upstream default branch; stamp shed.meta.

  local per repo (deterministic, retryable):
  5. Repair if needed: a checkout switched off its branch, a stale
     index.lock, a broken .git pointer — all put back automatically.
  6. Skip if already at the tracked ref — the common case.
  7. chmod u+w → fast-forward to the tracked branch (a force-pushed
     upstream is reported and hard-reset; a tracked tag only moves if the
     tag itself did) → chmod a-w.

Parallelism via --jobs (default 4), one job per mirror. Per-mirror locks
serialize concurrent syncs of the same upstream. Aggregate exit:
  5 if any lock acquisition timed out
  6 if any git fetch/clone failed (a "gone upstream" repo is not a failure)
  else 0

A tracked branch deleted upstream is reported as "track 'x' no longer
exists upstream". An upstream with no commits yet is the "empty" state,
not an error; the repo materializes once upstream gains commits.

The background variant ('shed __bg-sync', invoked by Claude's
SessionStart hook) wraps this with a global flock so multiple sessions
don't stampede.
`,

	"path": `path — print the absolute path of a repo or workspace by name

  shed path <name>
    Resolve a single name to a stored repo or a workspace and print its
    absolute path on stdout — nothing else. It changes no directory; compose
    it with cd:

      cd "$(shed path projects)"      # a repo (read-only store)
      cd "$(shed path my-workspace)"  # a workspace (writable)

One name, one path
  Repo names and workspace names share one namespace: a workspace can never
  have the same name as a repo, and a repo can never have the same name as a
  workspace (enforced when each is created). So a single name always resolves
  to exactly one path. A repo matches the way the rest of shed resolves names —
  an exact name, or an unambiguous trailing path segment (so "projects" finds
  "github.com/you/projects").

  Two repos may share a leaf under different owners — e.g.
  "github.com/alice/projects" and "github.com/bob/projects", which is allowed.
  A bare "projects" is then ambiguous and errors; pass the owner/repo form
  ("alice/projects"), or the full name, to choose one.

  The path is always absolute (never a "~"), so 'cd "$(shed path <name>)"'
  works without tilde-expansion surprises.

Exit 2 if the name matches nothing, matches more than one repo, or matches a
repo that has not been synced into the store yet.
`,

	"workspace": `workspace — manage writable workspaces

A workspace is a completely ordinary git repo: a plain local clone of a
repo's checkout (objects hardlink from the shared mirror, so creation is
fast and never blocked by the network) whose origin points at the real
upstream. Edits happen here.

  shed workspace new <repo> <name> [--base <branch|tag>]
    Always attempts a sync first so the workspace forks from the freshest
    code; if that fails (offline, auth, upstream down), it warns and forks
    from the last synced state — creation itself is purely local. If <name>
    exists as an upstream branch, check it out (any upstream branch works,
    fetched straight from the local mirror). Otherwise create it off
    <base>, defaulting to the repo's tracked branch or tag — a workspace
    from cpython@3.12 bases on 3.12. In a terminal the steps
    are narrated on stderr ("syncing <repo>...DONE", "Creating
    workspace: <path>"); when piped or captured, the bare absolute path is
    printed on stdout so command substitution works. Make changes there,
    then commit and push.

  shed workspace from-pr <pr> [--name <name>]
    Create a workspace holding an existing pull request's branch, for
    reviewing it or pushing fixes to it. <pr> is a PR URL
    (https://github.com/OWNER/REPO/pull/123) or a #-reference
    (OWNER/REPO#123, REPO#123); the repo must already be in the library.
    The workspace is named after the PR's head branch (--name overrides).
    With gh installed and authenticated, a same-repo PR checks its branch
    out tracking origin — 'git push' updates the PR — and a fork PR gets a
    second remote named "fork" pointing at the contributor's fork, tracked
    when reachable. Without gh, the workspace is named pr-<number> and
    holds the PR head (refs/pull/<n>/head, fetched from origin), with
    nothing wired for pushing back. Prints the path like 'new' does.

  shed workspace ls [--json]
    Every workspace with repo, branch, dirty state, unpushed-commit count,
    and last activity (its newest reflog entry — creation, commit, or
    checkout; the ACTIVE column).

  shed workspace rm <name>... [--force]
    Delete the named workspace dirs (names are globally unique). Several
    names may be given at once; each is removed independently, so a failure
    on one doesn't stop the rest. Refuses with exit 4 if dirty or unpushed
    unless --force.

The workspace's origin remote points at the upstream URL, not anything
shed-owned, so 'git push' works normally. New branches have no upstream
until your first 'git push -u origin <branch>'. Repos that use git LFS get
their blobs pulled right after creation (offline, you get pointer files
and a warning).

A workspace name must also differ from every repo name, so 'shed path <name>'
resolves unambiguously to one or the other — see 'shed help path'.

To bulk-clean workspaces whose work has already landed, see 'shed help prune'.
`,

	"prune": `prune — delete workspaces whose work has already landed

  shed prune [--dry-run] [--force] [--yes] [--if-older-than <dur>]
    Delete every workspace whose work has already landed, reclaiming the
    ones safe to delete. A workspace is reclaimed when its branch has a
    merged pull request (asked of GitHub via the gh CLI), or its own commits
    are already contained in the remote default branch (a merge- or
    rebase-merge with no PR). A workspace that never committed anything of
    its own is kept — an empty workspace has nothing to reclaim, so having
    no commits beyond the default branch is not on its own a reason to
    delete it. With --if-older-than, also reclaim workspaces whose last
    activity (newest reflog entry) is older than the given duration, e.g.
    --if-older-than 720h. Skips workspaces with uncommitted or unpushed
    changes so local work is never lost; pass --force to remove them anyway.
    Before deleting, prune lists the workspaces and asks for confirmation;
    pass --yes to skip the prompt or --dry-run to preview without deleting.

The merged-PR check is gh-driven, so gh must be installed and authenticated;
prune fails fast rather than degrade when gh can't report merge status.

prune is also shed's maintenance pass — you never run git upkeep by hand:
  1. Remove leftover repo checkouts no config entry claims (e.g. after
     changing a repo's track — an identity change leaves the old dir).
  2. git gc each mirror (after workspace removal, so less old pack data
     stays pinned by surviving hardlinked workspaces), plus stale-worktree
     bookkeeping cleanup.
  3. Remove mirrors that no config entry references — nothing else ever
     deletes a mirror.
The maintenance pass runs even when there are no workspaces (it doesn't
need gh), and --dry-run previews its removals too.
`,

	"history": `history — show recent shed commands

  shed history [-n <count>] [--json]
    Print recent shed commands, newest last. Default 20; -n/--limit
    changes how many. --json emits the raw events.

What's recorded
  Only "working" commands that change the library or workspaces are logged:
  add, rm, prune, init, and workspace new/from-pr/rm. Read-only queries (ls,
  status, workspace ls/path), background syncs, and the plain 'sync' command
  are not recorded, and only commands that succeed are. Each entry is the
  command exactly as you typed it, with a timestamp.

Storage and truncation
  Appended to ~/.shed/.internal/history.jsonl (one JSON object per line). The log
  is trimmed back to the most recent 200 entries, at most once every few
  minutes (a marker file debounces the trim), so it never grows without bound
  and the trim cost isn't paid on every command.
`,

	"agents": `agents — terminal coding agent integration

Supported (auto-detected by config-dir presence):

  claude       ~/.claude/             — Claude Code
  cursor       ~/.cursor/             — Cursor's CLI agent (cursor-agent)
  opencode     ~/.config/opencode/    — opencode

For claude, 'shed init' writes (idempotently, recorded in a
sidecar state file so 'shed init --uninstall' can reverse precisely):

  1. The repos + workspaces dirs in the allowed-filesystem-paths
     setting (permissions.additionalDirectories)
  2. A SessionStart hook running 'shed __session-context --agent
     <key>', which injects the shed guide into the session context.
     --agent selects the output shape that agent expects (the content is
     the same for all). The text is generated by the binary, so it never
     drifts after an upgrade.
  3. A SessionStart hook running 'shed __bg-sync', which refreshes
     the store in the background (skip with --no-bg-sync).

cursor is also a SessionStart-hook agent, but its hooks live in
~/.cursor/hooks.json under 'hooks.sessionStart' — a flatter, camelCase
shape than Claude's. 'init' adds the same two hooks there (session-
context + bg-sync); --agent cursor emits a '{"additional_context":"…"}'
JSON object Cursor injects into the conversation. Cursor has no path
allowlist (the chmod a-w on repos/ enforces read-only), so no paths are
registered.

opencode has no SessionStart hook or path allowlist, so 'init' instead
drops a plugin at ~/.config/opencode/plugin/shed.js. It runs
'shed __bg-sync' at startup and injects the guide ('shed
__session-context --agent opencode', the raw body) into the model's
system prompt. 'shed init --uninstall' deletes the file.
`,

	"auth": `auth — shed delegates to git

Shed does not manage credentials. Every git operation defers to
whatever 'git clone <url>' already works with in your shell:

  HTTPS  — credential helper ('gh auth setup-git',
           'git-credential-manager', OS keychain helpers)
  SSH    — your ssh-agent for git@github.com:... URLs

If 'git clone <url>' works in your shell, shed works. If it
doesn't, sync exits 6 with the underlying git error.
`,

	"locking": `locking — deadlock-free by design

Three lock scopes:

  global bg-sync  ~/.shed/.internal/bg-sync.lock
                  exclusive, non-blocking. Held only by __bg-sync workers.

  config          ~/.config/shed/.lock
                  exclusive, 2s timeout. Held briefly for config edits
                  (add/rm).

  per-mirror      <mirror>/.git/shed.lock (plus a sibling
                  <mirror>.create.lock that serializes first creation)
                  exclusive for sync phases (5min timeout) and for
                  worktree operations and gc (short timeouts — 2s in rm,
                  5s in prune, which skip a busy mirror rather than wait);
                  shared (2s timeout) for workspace creation. Sync
                  releases it between the network phase and each repo's
                  local update. 'workspace new' uses two separate
                  acquisitions (sync, then clone), never an in-place
                  upgrade.

Fixed acquisition order: bg-sync → config → per-mirror. No code path
acquires in reverse. flock auto-releases on process exit (including
SIGKILL).

The read-only enforcement (chmod -R a-w on each repo tree) excludes its
.git pointer; the mirror — where the lockfile and metadata sidecar live —
is never chmod'd. Sync re-enables write on a repo's tree only for the
moment it fast-forwards, then locks it again.

The chmod is a UX gotcha for cleanup: 'rm -rf ~/.shed/
repos/<name>' will fail. Run 'chmod -R u+w' first (or 'shed rm', which
handles it).
`,
}
