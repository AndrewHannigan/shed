# Design: mirrors — offline workspace creation and multi-version checkouts

Status: proposal
Author: design discussion (branch `claude/workspace-cached-repo-creation-bi8yna`)

## Invariant

Only shed writes to mirrors and catalog repos; agents only ever write to
workspaces. That write barrier is the design's dividing line: the
shed-written tiers (mirror, catalogs) share state for efficiency, and the
agent-written tier (workspaces) is isolated for durability.

## Goal

Make workspace creation a purely local operation, and allow multiple
read-only checkouts of the same upstream at different refs.

Today `shed workspace new` runs `git clone --reference <store> -- <upstream-url>
<dest>` — the store accelerates the clone via alternates, but the clone itself
still contacts the network. That means workspace creation is slower than it
needs to be and impossible offline, even though every object it needs is
usually already on disk.

## Requirements, as the user

Written user-first, mechanism-free. Each maps to the design section that
satisfies it (in parentheses).

**Reading code**

1. I can read any repo I track without cloning or pulling anything myself —
   there is always a browsable copy on my machine. (catalog repos)
2. If I track a **branch**, my copy has the latest changes after every
   sync; I never run `git pull` and it is never behind by more than one
   sync interval. (sync flow)
3. If I track a **tag**, my copy never changes — a tag is a permanent
   pointer, and my checkout behaves like one. (track semantics)
4. I can keep several versions of the same repo side by side — e.g.
   Airflow `main` and Airflow 2.7 — each independently referenceable by me
   and by agents. (multi-checkout, `@` naming)
5. When I `cd` into a copy and ask git where I am, the answer names the
   branch or tag I'm tracking, not just a hash. (branch worktrees)
6. Agents can create a workspace instantly, without touching the network —
   including fully offline (from the last sync). (workspace creation)
7. A workspace is a completely ordinary git repo: normal branching,
   committing, pushing to the real upstream, and its removal is trivial.
   (hardlink clones, `remote set-url`)
8. Nothing shed ever does in the background — sync, gc, prune — can
   corrupt, delete, or silently lose work sitting in a workspace. Ever.
   (isolation invariant, pre-receive hook, prune guards)
9. I never invent names: tracking `apache/airflow` at `v2-7-stable` names
   itself. (derived naming)
10. I never run git maintenance: no gc, no repack, no worrying about a
    years-old checkout getting slow or huge. Shed owns upkeep and does it
    at a moment I can see (`shed prune`). (gc ownership)
11. I only ever think about two things: repos I read and workspaces
    agents write in. Any machinery behind them stays invisible unless I go
    looking. (two-concept model, `.internal/`)
12. Tracking N versions of a big repo does not cost N copies of its
    history, and disk usage does not creep without bound as upstream
    churns. (worktrees, one object DB per upstream)
13. Syncing costs one network fetch per upstream, no matter how many
    versions I track. (mirror)
14. When something is wrong — a tracked branch deleted upstream, a repo
    unsyncable, a sync failure — shed tells me in plain language, and a
    broken shed-owned artifact repairs itself rather than wedging.
    (validation, repair passes, empty/zombie states)
15. Cleanup only ever reclaims what is finished: merged, landed, or aged
    out — and refuses to touch anything dirty or unpushed without `--force`.
    (prune, unchanged from today)

Requirement 8 is the charter: where it conflicts with anything else, it
wins. Requirements 6, 12, and 13 are why mirrors exist; 2–4 are why
catalog repos exist as their own tier; 10 and 14 are why shed, not git
defaults, owns maintenance and repair.

## The three-tier model

| Tier | What it is | Writable by | Lifetime | Created by |
|---|---|---|---|---|
| **mirror** | fetch-only repo, tree never checked out; upstream truth in `refs/remotes/origin/*` | shed (network fetch) | permanent, one per upstream URL | derived — never configured directly |
| **catalog repo** | worktree of the mirror on a local branch (or detached at a tag) | shed (ff-merge on sync) | permanent, N per mirror | config (`[[repos]]`) |
| **workspace** | plain local clone off a catalog repo, origin → upstream | agents | disposable | agents (`shed workspace new`) |

```
GitHub ──fetch──▶ Mirror ──ff──▶ Catalog worktrees ──clone──▶ Workspaces
                                                       │
Agents ◀──────────── push to origin (GitHub) ◀─────────┘
```

Only two of the three are user-facing vocabulary: users add **repos**
(things you read — "catalog repo" is this doc's precise term; the CLI just
says repo) and agents make **workspaces** (things you write). The
**mirror** is plumbing that surfaces only in `shed sync` output, debug
messages, and docs. Mirrors are **not a config entity** — created on
demand, one per unique upstream, shared by every catalog repo pointing at
that upstream.

### The ref-namespace split is the load-bearing trick

The mirror fetches with the **standard clone refspec**, not a mirror
refspec:

```
fetch = +refs/heads/*:refs/remotes/origin/*
fetch = +refs/tags/*:refs/tags/*          # forced; --prune --prune-tags
```

Upstream truth lands in `refs/remotes/origin/*`. The mirror's *local*
branch namespace (`refs/heads/*`) contains exactly one branch per
branch-tracked catalog repo, checked out by that repo's worktree.

This split is what makes the design safe by construction:

- **Fetch can never be blocked.** Git refuses to fetch into a checked-out
  branch — but sync only fetches into `refs/remotes/*`, which no worktree
  can have checked out. Verified: fetch succeeds with catalog branches
  checked out, and even with a catalog left in a mischief state. Agent
  behavior inside a catalog can break *that catalog's next update*, never
  the fetch, never sibling catalogs.
- **Catalogs sit on real branches.** `git status` says `On branch main`;
  prompts show the branch; `git log` decorates `(HEAD -> main,
  origin/main)`. The detached-HEAD UX concern from earlier iterations
  dissolves. (Tag-tracked catalogs are necessarily detached — git cannot
  be "on" a tag — and get a named detach so status reads
  `HEAD detached at 2.7.3`.)
- **Every entity is a standard git construct**: a bare repo with a normal
  remote layout, worktrees on branches, ff-only merges, plain clones. No
  bespoke ref layouts, no alternates, no `.keep` files.

### Mirror creation spec

The mirror is a normal (non-bare) repo whose working tree is **never
checked out** and whose HEAD is **detached** — it must not occupy any
branch, since `refs/heads/*` belongs to the catalogs. Bare would also
work, but non-bare is simpler: it dodges two bare-repo footguns outright
(`extensions.worktreeConfig` requires `core.bare` relocation on bare
repos or it bricks every worktree, and bare defaults
`core.logAllRefUpdates` off, breaking reflog-based labels), and it keeps
the `.git/`-sidecar layout of today's store code. All verified.

Creation = `git clone --no-checkout` (into a temp dir, renamed into place
— a kill -9 mid-creation must not leave a half-mirror that later syncs
trust), which is today's store-creation verb: it sets the remote-tracking
fetch refspec and `origin/HEAD` automatically, takes `--config` seeds, and
reuses the existing progress-streaming clone path. Two fixups follow
(both verified): clone creates a local default branch with HEAD attached
to it — squarely in the catalogs' namespace — so `update-ref --no-deref
HEAD <tip>` detaches HEAD without touching the tree, then `branch -D
<default>` frees the branch name for its catalog worktree. The tag
refspec is added alongside the default one. Each sync re-points the
detached HEAD ref-only; the working tree is never materialized. There is
no consumer for a mirror working tree — the default-branch catalog is the
visible checkout — so checking one out would burn checkout IO per sync
and a full tree of disk for nothing.

Creation-time config:

- **`extensions.worktreeConfig=true`** — per-repo `Git` config on
  catalogs without leaking to the mirror or siblings; on a non-bare repo
  this needs no `core.bare` surgery (verified).
- **`gc.auto=0`** — not for safety (see the gc section: nothing left to
  corrupt) but for timing ownership: shed does the mirror's maintenance
  in prune, on shed's schedule.
- **A `pre-receive` hook rejecting all pushes.** A half-created workspace
  (crash between clone and `remote set-url`) has a shed-owned path as
  `origin`. Pushes to a checked-out catalog branch are refused natively
  (verified), but pushes to *other* branch names land in the mirror's
  `refs/heads/*` (verified) — stray state in a namespace shed owns. One
  `exit 1` file closes the hole.

**Mirror identity.** "One per upstream" is keyed by the URL-derived
`host/owner/repo` path, not the raw URL string. Two config entries for one
upstream over different transports (`https://…` and `git@…:`) share a
mirror; it fetches with the first entry's URL, and config validation warns
when entries sharing a mirror disagree on transport.

## Config: the `track` field

`Repo` gains one optional field:

```toml
[[repos]]
url = "https://github.com/apache/airflow"
# track defaults to the upstream default branch

[[repos]]
url = "https://github.com/apache/airflow"
track = "v2-7-stable"          # a branch: advances on every sync

[[repos]]
url = "https://github.com/apache/airflow"
track = "2.7.3"                # a tag: never changes
```

- **branch** — the catalog worktree sits on local branch `<track>`,
  fast-forwarded to `origin/<track>` on every sync.
- **tag** — a detached worktree at the tag; sync is a no-op unless the tag
  itself moved (the forced tag refspec propagates moves/deletions; the old
  checkout keeps working — its HEAD is a gc root — and `shed ls` flags the
  divergence).

**One repo per `(url, track)` is an invariant**, enforced by
`config.Validate` even under explicit `name` overrides: two checkouts of
one ref would be identical read-only trees, and the invariant is also what
maps catalog branches 1:1 onto worktrees (git itself then refuses a second
checkout of the same branch — belt and suspenders).

Branch/tag name collisions: `track` accepts full-ref forms (`heads/2.7.3`,
`tags/2.7.3`) as the escape hatch; bare short names prefer branches,
matching `git clone --branch` (verified). Full-ref forms are for
resolution only — workspace creation normalizes to short names.

`track` values get `ValidateBranch`-style validation (no leading `-`, safe
relative path) before reaching any git command. Sync pre-checks the
tracked ref still exists upstream so a deleted branch yields "track
'feature/x' no longer exists upstream" rather than a git internals error.

Per-repo `Git` config: seeded into workspaces via `--config` at clone
time; set on catalog repos via `extensions.worktreeConfig` so values never
leak to the mirror or sibling catalogs (verified isolated).

Owner auto-discovery always materializes default-branch repos; `track`
overrides are added by hand, never auto-generated.

## Naming: derived, never required

Unchanged from earlier iterations:

- default branch → `github.com/apache/airflow`
- tracked ref → `github.com/apache/airflow@v2-7-stable`
- slashes in the track portion sanitize to `-` at name-derivation time
  (`release/2.8` → `airflow@release-2.8`, one leaf dir); the mapping is
  lossy but config is the source of truth; `config.Validate` rejects
  names that sanitize identically.
- the optional `name` field remains as an override, never demanded.
- changing `track` is an identity change (remove-and-add); sync notices
  on-disk dirs with no config entry and offers to prune them.

## Path layout

```
~/.shed/
├── repos/                                    # user/agent-facing: shed prints these paths
│   └── github.com/apache/
│       ├── airflow/                          # on branch main — ff'd on sync
│       │   └── .git                          # a FILE → worktree of the mirror
│       ├── airflow@v2-7-stable/              # on branch v2-7-stable
│       └── airflow@2.7.3/                    # detached at tag 2.7.3 — frozen
├── workspaces/                               # user/agent-facing
│   └── github.com/apache/
│       ├── airflow/fix-dag/
│       └── airflow@v2-7-stable/fix-dag/      # same ws name, no collision
├── logs/                                     # user-serviceable when debugging
└── .internal/                                # plumbing — never printed as a destination
    ├── mirrors/
    │   └── github.com/apache/airflow/        # fetch-only, tree never checked out;
    │       └── .git/{shed.lock,shed.meta}    #   sidecars in .git as today
    ├── sync-errors/                          # was .sync-errors/ — dot dropped
    ├── sessions-pending/
    ├── bg-sync.lock
    ├── history.jsonl
    └── history-trim
```

- **One `.internal/` bucket, one rule**: if shed prints a path for the
  user or an agent to visit, it's top-level; everything else lives under
  `.internal/`. `logs/` stays out because it exists for the user.
- **Mirror sidecars live in its `.git/`** (`shed.lock`, `shed.meta`),
  matching today's store pattern; the mirror's meta owns
  `LastSyncAt`/`LastError` and per-catalog records (keyed by repo name —
  **not** in git's `worktrees/<id>/` admin dir, which `git worktree prune`
  deletes exactly when a broken repo's record matters).
- A catalog repo's `.git` is a pointer file into the mirror; per-repo
  state beyond that is nearly nil (the branch is the state; identity is in
  config).

## Flows

### Sync

```
per mirror (network, exclusive lock):
  1. git fetch '+refs/heads/*:refs/remotes/origin/*' \
       '+refs/tags/*:refs/tags/*' --prune --prune-tags   ← the only network step
     (structurally unblockable: no worktree can have refs/remotes/* checked
      out, so agent mischief in catalogs can never fail the fetch)
  2. refresh refs/remotes/origin/HEAD to upstream default
     (git ls-remote --symref); re-point the mirror's detached HEAD to the
     default tip ref-only (update-ref --no-deref — no checkout, ever)
  3. write shed.meta
  — release the exclusive lock between phases —

per catalog repo (local, deterministic, retryable; exclusive lock per repo):
  4. repair if needed: worktree not on its designated branch (agent ran
     checkout) → re-checkout <track>; stale index.lock → remove (kill -9
     mid-update is routine and git never cleans it); stray local branches
     in the mirror not matching any catalog → delete
  5. skip if already at origin/<track> — the common case; avoids two
     full-tree chmod walks
  6. unlock tree → git merge --ff-only origin/<track> → lock tree
     (force-pushed upstream → ff fails → git reset --hard origin/<track>,
      reported in plain language)
  tag-tracked: detached; steps 5–6 fire only if the tag itself moved
```

The forced tag refspec is required, not optional: default tag handling
refuses a moved tag (`would clobber existing tag`), failing every
subsequent sync of that mirror forever, and `--prune` alone never removes
upstream-deleted tags (both verified).

### Workspace creation

```
1. optional best-effort mirror fetch + catalog ff (same
   warn-and-proceed-if-stale fallback as today; hard-fail only if no
   mirror exists at all)
2. git clone --branch <base> [--config k=v ...] -- <catalog-path> <dest>
   (objects hardlink from the mirror store through the worktree — verified)
3. git remote set-url origin <upstream-url>
4. new-branch case: git checkout -b <name>, as today
```

- **Base branch defaulting**: a workspace created via
  `airflow@v2-7-stable` bases on `v2-7-stable` — the catalog's branch is
  what its clone naturally checks out.
- **Tags**: clone from a tag-tracked catalog uses `--branch <tag>`
  (detached + `checkout -b`; verified from local sources).
- **Arbitrary non-catalog branches** (e.g. reviewing a colleague's
  feature branch): a plain clone only sees the source's local branches, so
  this is a two-step — clone the catalog, then
  `git fetch <mirror-path> '+refs/remotes/origin/<br>:refs/remotes/origin/<br>'`
  and `checkout -b <br> origin/<br>`. Still offline, still cheap
  (verified); the fetched delta is copied not hardlinked, which is fine at
  workspace lifetimes.
- **Crash window**: a crash between steps 2 and 3 leaves origin pointing
  at a shed-owned path; the mirror's pre-receive hook makes any push there
  fail loudly, and `workspace new` on an already-existing directory
  validates its origin URL and repairs or replaces. Cleanup at any step is
  `rm -rf <dest>`. Clone failures retry once (insurance against losing a
  race with a rogue repack — see gc).
- **LFS**: the mirror never smudges, so it has pointer files only; clone
  with `GIT_LFS_SKIP_SMUDGE=1`, then after set-url run `git lfs pull` when
  the repo uses LFS. Offline LFS workspaces get pointer files and say so.
- Freshness note: step 1 updates mirror + source catalog, not sibling
  catalogs. Accepted — catalogs advance on sync.

### gc: safe everywhere, scheduled where shed does the maintaining

Corruption is off the table on every tier, by construction:

- **Mirror** — catalog branches, tag refs, remote-tracking refs, and
  worktree HEADs are all gc reachability roots, so a collection at any
  moment (including an agent poking a catalog) can never prune what a
  catalog needs — verified through `gc --prune=now --aggressive` after an
  upstream force-push. Workspaces are independent hardlink clones: a
  mirror repack costs disk *sharing*, never validity.
- **Catalogs** — no object store of their own; nothing to collect.
- **Workspaces** — gc repacks only the workspace's own clone.

Given that, `gc.auto` splits by who does the maintenance:

- **Mirror pins `gc.auto=0`** — not for safety, for timing ownership.
  Shed maintains the mirror, so shed schedules it: prune's repack
  ordering stays meaningful, workspace clones never race a repack (gc
  runs only under the exclusive lock, demoting the clone retry-once to
  insurance against agent-run gc), bg-sync latency stays flat instead of
  absorbing a detached auto-gc after a big fetch, and failures surface in
  prune output rather than as a silent `gc.log` that quietly blocks all
  future auto-gc.
- **Workspaces stay stock** — shed never maintains them, so there is no
  shed timing to transfer; git's own auto-gc keeps a long-lived workspace
  healthy like any ordinary clone. The worst case is a disk cost, not a
  correctness one: an auto-gc repack rewrites the hardlinked base into a
  private copy, reclaimed when the workspace is pruned. Accepted — the
  price of workspaces being 100% ordinary git repos.

`shed prune` runs the mirror's maintenance:

1. remove landed/aged workspaces (as today)
2. `git gc` each mirror under its exclusive lock — repacking *after*
   removal minimizes how much old pack data surviving hardlinked
   workspaces keep pinned
3. `git worktree prune` each mirror
4. remove mirrors no config entry references — nothing else deletes them

The underlying asymmetry that shaped the tiers stands: **clone hardlinks,
fetch copies** — fine for the ephemeral tier, and why the permanent tiers
are worktrees instead.

### Catalog removal, zombies, empty upstreams

- Removal is **unlock tree → `git worktree remove`**, in that order:
  against a locked tree, remove deregisters *first* then fails deletion
  (verified), leaving a zombie — read-only dir, dangling `.git` pointer,
  invisible to `worktree list`/`prune`, present to a naive stat.
- Validity is therefore "`.git` pointer resolves", not "directory exists";
  sync repairs invalid catalogs: chmod +w, `rm -rf`, re-add (verified
  clean on git 2.43; older gits need a prune — set a version floor).
- **Empty upstream** (no commits): nothing to check out; the repo entry
  holds an "empty" state with no directory, reported as such, not as an
  error; materializes on the first sync after upstream gains commits.

## Alternatives considered

The design iterated through, in order — each rejected for the reason
given:

1. **Hardlink clones for the read-only tier.** Fetch copies: an advancing
   checkout accumulates upstream churn privately forever (~5 GB/year per
   checkout on a busy monorepo) plus a pack per fetch; gc inside the clone
   privatizes the hardlinked base (disk shock) or demands `.keep`
   bookkeeping — hand-reimplementing the dependency tracking worktrees
   give natively. Repointing origin at upstream changes nothing (repos
   never push; fetching upstream directly breaks one-fetch-per-upstream).
2. **`--shared` clones (alternates).** Zero duplication but invisible to
   git: mirror gc doesn't know the repo exists → force-push + prune race →
   defensive scheduling + broken-repo re-derive logic.
3. **No mirror at all (two tiers).** Each repo becomes a non-bare mirror
   wearing a checkout: N network fetches per upstream (breaks req 13),
   fetch-copies churn (breaks req 12), and most hardening survives anyway.
   The mirror is the cheap part.
4. **Detached worktrees of a branch-mapped mirror** (the previous
   iteration of this doc: `+refs/heads/*:refs/heads/*`, catalogs as
   detached worktrees). Sound — all mechanics verified — but strictly
   dominated by the current design: it needed a pre-fetch detach-repair
   pass because *one* attached branch anywhere failed the whole mirror
   fetch (verified, wholesale), and it surfaced detached-HEAD UX in every
   catalog. Moving upstream truth to `refs/remotes/origin/*` makes the
   fetch structurally unblockable, puts catalogs on real branches, and
   demotes agent mischief from "sync-wide outage" to "one catalog repairs
   itself on next sync". Its cost: workspaces on non-catalog branches are
   a two-step instead of one `clone --branch` (still offline). Also
   rejected along the way: shed-owned `shed/<name>` prompt-branches and
   pull-through-worktree (per-catalog network fetches).

The README's "why not `git worktree`" section is re-scoped, not reversed:
its arguments (user work, pushing, independent teardown) are all true of
workspaces — which remain plain clones — and none true of catalogs.

## No migration

shed is unreleased; the new layout lands as *the* layout in one change.
Old `~/.shed/repos/` contents are invalid — blow away and re-sync.
`pkg/repostore` splits into mirror and catalog packages; the README's
design-rationale sections are rewritten for this model.

## Verified behaviors (git 2.43)

Two empirical passes (an adversarial red-team of iteration 4, plus direct
tests of the current scheme):

- fetch into `refs/remotes/origin/*` succeeds with catalog branches
  checked out and with a catalog deliberately detached — agent mischief
  cannot block it
- `git worktree add -b <branch> <path> origin/<branch>` from a bare repo
  works; `merge --ff-only origin/<branch>` inside the worktree advances
  branch + tree
- `git clone <catalog-worktree-path>` works and hardlinks the mirror's
  objects (st_nlink observed rising); clone sees the source's local
  branches only — the basis of the two-step for non-catalog branches
  (also verified working, offline)
- push to a checked-out catalog branch is refused natively; push to other
  names lands in mirror `refs/heads/*` → hook required
- the non-bare, never-checked-out, detached-HEAD mirror works end to end:
  `init` + fetch leaves an empty working tree; worktree add, per-worktree
  config (no `core.bare` surgery — that relocation is only needed on
  *bare* repos, where skipping it bricks every worktree), fetch with
  catalog checked out, ff-merge, and clone-from-catalog all pass
- worktree HEADs/branches survive `gc --prune=now --aggressive` after
  upstream force-push; hardlink clones; `clone --branch <tag>` from local
  sources; moved-tag clobber failure and its forced-refspec fix;
  `worktree remove` zombie on locked trees; workspace clones get
  `refs/remotes/origin/HEAD` so landed-work prune logic survives
  `remote set-url`

## Open questions / remaining notes

1. **Hardlink behavior across filesystems** — local clone falls back to
   copying when `~/.shed` spans devices; correct, just slower. Doc note.
2. **Mirror HEAD refresh cadence** — `ls-remote --symref` per sync;
   confirm cheap or gate on ref changes.
3. **Lock ordering** — shared (workspace creation) vs exclusive (sync
   phases, worktree ops, gc); `workspace new` uses two separate
   acquisitions, never an in-place flock upgrade (deadlocks).
4. **chmod walk with a `.git` file** — today's `chmodTree` returns
   `filepath.SkipDir` at `.git`; from a non-dir entry that skips the rest
   of the parent dir, silently leaving most of the repo root writable.
   Return nil for the pointer file.
5. **Case-insensitive filesystems** — all upstream branches materialize
   as refs (`Foo`/`foo` collide on APFS/NTFS — same exposure as today);
   `@`-names differing only by case collide as dirs.
6. **Disk reporting** — real bytes live under `.internal/mirrors/`;
   `shed ls`/sync need a story (per-mirror size + tree-only per repo).

## Implementation sequence

1. **Paths + config.** `.internal/` root and `mirrors/` helpers; relocate
   plumbing paths; `Track` on `Repo`; `@` name derivation + sanitization;
   `Validate`: name uniqueness, sanitized-path uniqueness, `(url, track)`
   uniqueness, shared-mirror transport warning; track validation.
2. **Mirror package.** `git clone --no-checkout` in temp-then-rename
   (today's creation verb, keeps progress streaming) + detach HEAD
   ref-only + delete the clone-created default branch; add forced tag
   refspec; `--prune --prune-tags` fetch; origin/HEAD refresh; creation
   config (worktreeConfig, gc.auto=0 for timing ownership); pre-receive
   reject hook; canonical identity; lock/meta in `.git/`.
3. **Catalog package.** Create = `worktree add -b <track>
   origin/<track>` (branch) or `worktree add --detach` + named checkout
   (tag) → tree lock; update = repair pass (wrong branch, stale
   index.lock, stray mirror branches), skip-if-current, unlock →
   `merge --ff-only` (reset --hard on force-push) → lock; removal =
   unlock → `worktree remove`; validity = `.git` pointer resolves, with
   repair; empty-upstream state; per-repo config via worktreeConfig.
4. **Sync rewrite.** Fetch mirror → update catalogs, lock released
   between phases; one fetch per mirror across N catalogs; meta on the
   mirror.
5. **Workspace creation rewrite.** Clone from catalog
   (`GIT_LFS_SKIP_SMUDGE=1`, retry once) + `remote set-url` + `lfs pull`
   when applicable; two-step path for non-catalog branches; origin
   validation on already-exists; base defaulting from the source
   catalog's track.
6. **Prune gains gc.** Workspace removal → mirror gc under exclusive
   lock → `worktree prune` → orphan-mirror removal; orphan catalog-dir
   detection.
7. **CLI + resolution.** `@` names through `Resolve`; `shed add --track`;
   sync output mentions mirrors; `shed ls` TRACK/SYNCED columns.
8. **Docs.** README model rewrite; agent guide: repos are read-only
   including their git plumbing.
