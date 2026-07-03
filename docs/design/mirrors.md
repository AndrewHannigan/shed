# Design: mirrors — offline workspace creation and multi-version checkouts

Status: proposal
Author: design discussion (branch `claude/workspace-cached-repo-creation-bi8yna`)

## Goal

Make workspace creation a purely local operation, and allow multiple
read-only checkouts of the same upstream at different refs.

Today `shed workspace new` runs `git clone --reference <store> -- <upstream-url>
<dest>` — the store accelerates the clone via alternates, but the clone itself
still contacts the network. That means workspace creation is slower than it
needs to be and impossible offline, even though every object it needs is
usually already on disk.

The end state:

- `shed workspace new` works instantly and offline (given a previously synced
  mirror).
- A user can keep several read-only checkouts of one upstream — e.g. Airflow
  `main`, the `v2-7-stable` branch, and the `2.7.3` tag — side by side, each
  independently referenceable by agents.
- Sync does one network fetch per upstream, no matter how many checkouts
  derive from it.

## Requirements, as the user

Written user-first, mechanism-free. Each maps to the design section that
satisfies it (in parentheses).

**Reading code**

1. I can read any repo I track without cloning or pulling anything myself —
   there is always a browsable copy on my machine. (repos)
2. If I track a **branch**, my copy has the latest changes after every
   sync; I never run `git pull` and it is never behind by more than one
   sync interval. (sync flow)
3. If I track a **tag**, my copy never changes — a tag is a permanent
   pointer, and my checkout behaves like one. (track semantics)
4. I can keep several versions of the same repo side by side — e.g.
   Airflow `main` and Airflow 2.7 — each independently referenceable by me
   and by agents. (multi-checkout, `@` naming)
5. When I `cd` into a copy and ask git where I am, the answer names the
   branch or tag I'm tracking, not just a hash. (browsing UX)

**Writing code**

6. Agents can create a workspace instantly, without touching the network —
   including fully offline (from the last sync). (workspace creation)
7. A workspace is a completely ordinary git repo: normal branching,
   committing, pushing to the real upstream, and its removal is trivial.
   (hardlink clones, `remote set-url`)
8. Nothing shed ever does in the background — sync, gc, prune — can
   corrupt, delete, or silently lose work sitting in a workspace. Ever.
   (sharing-mechanism-per-tier rule, pre-receive hook, prune guards)

**Not my job**

9. I never invent names: tracking `apache/airflow` at `v2-7-stable` names
   itself. (derived naming)
10. I never run git maintenance: no gc, no repack, no worrying about a
    years-old checkout getting slow or huge. Shed owns upkeep and does it
    at a moment I can see (`shed prune`). (gc ownership)
11. I only ever think about two things: repos I read and workspaces
    agents write in. Any machinery behind them stays invisible unless I go
    looking. (two-concept model, `.internal/`)

**Resources**

12. Tracking N versions of a big repo does not cost N copies of its
    history, and disk usage does not creep without bound as upstream
    churns. (worktrees, one object DB per upstream)
13. Syncing costs one network fetch per upstream, no matter how many
    versions I track. (mirror)

**Trust**

14. When something is wrong — a tracked branch deleted upstream, a repo
    unsyncable, a sync failure — shed tells me in plain language, and a
    broken shed-owned artifact repairs itself rather than wedging.
    (validation, repair passes, empty/zombie states)
15. Cleanup only ever reclaims what is finished: merged, landed, or aged
    out — and refuses to touch anything dirty or unpushed without `--force`.
    (prune, unchanged from today)

Requirement 8 is the charter: where it conflicts with anything else, it
wins. Requirements 6, 12, and 13 are why mirrors exist; 2–4 are why repos
exist as their own tier; 10 and 14 are why shed, not git defaults, owns
maintenance and repair.

## Background: why the previous local-clone attempt was messy

An earlier exploration of clone-from-store failed for a structural reason,
not an incidental one. The store is a **non-bare** clone, so upstream branches
exist only as remote-tracking refs (`refs/remotes/origin/*`). A local
`git clone --branch feature-x <store>` cannot see those — clone only offers a
source repo's *local* branches — so creating a workspace on an arbitrary
branch required init + fetch + checkout by hand, with partial-failure rollback
at each step.

A **mirror** inverts that: a bare repo whose fetch refspec maps upstream
branches to local branches (`+refs/heads/*:refs/heads/*`). Every upstream
branch is a local branch in the mirror, so
`git clone --branch <anything> <mirror> <dest>` is a single git command again
— the same shape as today's network clone, with a local source.

### Object sharing: hardlinks for workspaces, worktrees for repos

Today every workspace is permanently welded to the store through
`.git/objects/info/alternates` (the `--reference` clone has no `--dissociate`).
That is why the store runs `gc.auto=0` forever: a repack in the store would
corrupt every workspace.

The new design picks a mechanism per tier by two questions: does the tier
hold user work (then it must be independent of the mirror), and is it
long-lived (then git itself must know it depends on the mirror, so
maintenance respects it)?

- **Workspaces** hold user work — corruption is unacceptable, so they get a
  plain local clone, no `--reference`. Git hardlinks the object files: same
  speed and near-zero disk (on one filesystem), but fully independent — the
  mirror can be repacked, rebuilt, or deleted and workspaces stay valid.
  Worst case sharing degrades and disk creeps until old workspaces are
  pruned — acceptable precisely because workspaces are ephemeral.
- **Repos** are long-lived, shed-owned, read-only, and never push — so they
  are **detached worktrees of the mirror** (`git worktree add --detach`).
  One object DB, zero duplication ever, and — decisively — **gc-safety by
  construction**: the mirror's gc counts every worktree's HEAD as a
  reachability root, so it can never prune objects a repo's checkout still
  needs. The dependency is declared to git rather than managed by shed: no
  `.keep` bookkeeping, no defensive gc scheduling, no broken-repo re-derive
  path. (See "gc: owned by shed, run in prune" for the full accounting.)

## The three-tier model

| Tier | What it is | Writable? | Lifetime | Created by |
|---|---|---|---|---|
| **mirror** | bare repo, all upstream branches + tags | network only | permanent, one per upstream URL | derived — never configured directly |
| **repo** | read-only detached worktree of the mirror at a tracked ref | no (tree locked) | permanent, N per mirror | config (`[[repos]]`) |
| **workspace** | writable clone from the mirror | yes | disposable | agents (`shed workspace new`) |

Only two of the three are user-facing vocabulary. Users add **repos** (things
you read) and agents make **workspaces** (things you write); the **mirror** is
plumbing that surfaces only in `shed sync` output, debug messages, and docs.
If a user tracks one version of one upstream — the overwhelmingly common case
— they never need to know mirrors exist.

Mirrors are therefore **not a config entity**. They are created on demand, one
per unique upstream URL, shared by every repo that points at that URL.

### "Mirror" is colloquial, not `git clone --mirror`

Literal `--mirror` fetches `refs/*`, which on GitHub includes `refs/pull/*` —
enormous bloat for active repos. The mirror is instead a bare clone with
explicit refspecs:

```
fetch = +refs/heads/*:refs/heads/*
fetch = +refs/tags/*:refs/tags/*
```

fetched with `--prune --prune-tags`. The tag refspec must be **forced** and
tag-pruned: git's default tag handling refuses a moved tag (`would clobber
existing tag`), which would fail every subsequent sync of that mirror
forever, and `--prune` alone never removes upstream-deleted tags. With the
forced form the mirror's tags exactly track upstream (verified, git 2.43).

### Mirror creation spec

Several bare-repo defaults are wrong for this topology and are set at
creation (all verified):

- **`extensions.worktreeConfig=true`, with `core.bare` relocated to the
  mirror's own `config.worktree`.** Enabling the extension naively bricks
  every linked worktree (`fatal: this operation must be run in a work
  tree` — the shared `core.bare=true` leaks into them). Relocation is the
  documented fix; doing it unconditionally at creation means per-repo
  `Git` config can be applied later with no migration.
- **`core.logAllRefUpdates=true`** — bare repos default it off, which
  leaves worktrees without HEAD reflogs and degrades the detached-HEAD
  status label (see the browsing-UX section).
- **`gc.auto=0`** (see the gc section).
- **A `pre-receive` hook that rejects all pushes.** A half-created
  workspace (crash between clone and `remote set-url`) has the mirror as
  `origin`; without the hook an agent's push into the bare mirror
  *succeeds* and the next `--prune` fetch silently deletes the pushed ref
  (verified) — silent work loss, the design's forbidden failure mode. One
  `exit 1` file closes it.
- **Clone into a temp dir, rename into place.** Today's store code treats
  any existing directory as a valid store; a kill -9 mid-clone must not
  leave a half-mirror that every later sync trusts and fails against.

**Mirror identity.** "One per upstream URL" needs a canonical key: the
URL-derived `host/owner/repo` path, not the raw URL string. Two config
entries for one upstream over different transports (`https://…` and
`git@…:`) map to the same mirror — legal, but the mirror fetches with one
URL: shed uses the first entry's and warns at config validation when
entries sharing a mirror disagree on transport (an SSH-only user's repo
silently fetching HTTPS is otherwise a baffling auth failure).

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
track = "2.7.3"                # a tag: effectively frozen; sync is a no-op
```

One field, accepting a branch or a tag (a commit SHA is cheap to allow later).
The semantics follow from what the ref names:

- **branch** — the repo advances on every sync: detached checkout at the
  mirror's current tip of that branch (exactly today's `origin/HEAD`
  behavior, generalized).
- **tag** — frozen; sync only re-checks-out if the tag itself moved.

In the rare case a branch and a tag share a name, `track` accepts the full
ref form (`heads/2.7.3`, `tags/2.7.3`) as the escape hatch; the bare short
name prefers branches, matching `git clone --branch` (precedence verified).
The full-ref forms are for resolution only — `git clone --branch` accepts
neither (verified), so workspace creation normalizes to the short name and
tracks whether a branch or tag won, since the clone shapes differ
(attached branch vs detached HEAD + `checkout -b`).

`track` values get `ValidateBranch`-style validation (no leading `-`, safe
relative path) before reaching `git checkout --detach` or `git clone
--branch`. Sync pre-checks that the tracked ref still exists so a deleted
upstream branch yields "track 'feature/x' no longer exists upstream"
rather than git's baffling `--detach does not take a path argument`. A
moved or deleted *tag* does propagate (the mirror tracks tags exactly);
the previously checked-out tree keeps working because its detached HEAD is
a gc root, and `shed ls` should flag the divergence.

**One repo per `(url, track)` is an invariant**, enforced by
`config.Validate` even under explicit `name` overrides. Two repos of the
same upstream at the same ref would be identical read-only trees — pure
duplication with no use — and forbidding them is also what lets repos be
worktrees without ever meeting git's same-branch-twice restriction.

The existing per-repo `Git` config map is unchanged in meaning: it applies
per repo entity and is still seeded into workspaces at clone time via
`--config`. On repos — worktrees sharing the mirror's config file — per-repo
values are set via `extensions.worktreeConfig`, so they never leak to the
mirror or sibling repos.

Owner auto-discovery always materializes default-branch repos; a `track`
override is something the user adds by hand afterward, never auto-generated.

## Naming: derived, never required

The user never invents a name. `ResolvedName()` extends today's URL derivation
to include the track:

- default branch → `github.com/apache/airflow` (unchanged)
- tracked ref → `github.com/apache/airflow@v2-7-stable`

The `@ref` convention reads instantly (npm, Docker, Go modules), works with
the existing suffix resolution (`shed workspace new airflow@v2-7-stable
fix-dag`), and bare `airflow` keeps resolving to the default checkout — the
`@`-suffixed entries are distinct, non-competing names in the same global
namespace shared with workspaces.

The optional `name` field stays as an override for anyone who prefers
`airflow-27`, but is never demanded.

### Sanitization: the track portion of a name never nests

Slashes in the `host/owner/repo` part of a name mean directory nesting, as
today. Slashes in the track portion are sanitized to `-` **at name-derivation
time**, so the canonical name is already path-safe and every downstream
consumer (`RepoStorePath`-equivalent, lock files, sync-error keys, workspace
nesting, CLI resolution) keeps working on a single string:

- `track = "release/2.8"` → name `…/airflow@release-2.8` → one leaf dir
- `track = "tags/2.7.3"` → name `…/airflow@tags-2.7.3` → one leaf dir

The mapping is lossy (the dir name doesn't round-trip to the ref), which is
fine because config is the source of truth — name→track lookup always goes
through config, never by parsing paths. The cost is a collision check:
`config.Validate` must reject two repos whose names sanitize identically
(e.g. branches `release/2.8` and `release-2.8`), loudly, at config time.
`checkSafeRelPath` already permits `@`; `ValidateName` needs no loosening.

Because names are derived, **changing `track` is an identity change** —
effectively remove-and-add, leaving the old checkout as garbage. That is the
better contract (a repo pinned to a ref is immutable; its meaning can't
silently shift under agents holding references to it), but sync or a validate
pass should notice on-disk repo dirs with no config entry and offer to prune
them.

## Path layout

```
~/.shed/
├── repos/                                    # user/agent-facing: shed prints these paths
│   └── github.com/apache/
│       ├── airflow/                          # default branch — advances on sync
│       │   └── .git                          # a FILE → worktree of the mirror
│       ├── airflow@v2-7-stable/              # branch — advances on sync
│       ├── airflow@release-2.8/              # branch "release/2.8", sanitized
│       └── airflow@2.7.3/                    # tag — frozen
├── workspaces/                               # user/agent-facing
│   └── github.com/apache/
│       ├── airflow/fix-dag/                  # workspace off the default repo
│       └── airflow@v2-7-stable/fix-dag/      # same branch name, no collision
├── logs/                                     # user-serviceable when debugging
└── .internal/                                # plumbing — never printed as a destination
    ├── mirrors/
    │   └── github.com/apache/airflow.git/    # bare mirror, one per upstream URL
    │       ├── shed.lock                     # bare repo: sidecars at top level
    │       └── shed.meta                     # LastSyncAt / LastError live here
    ├── sync-errors/                          # was .sync-errors/ — dot dropped
    ├── sessions-pending/                     # was .sessions-pending/
    ├── bg-sync.lock                          # was .bg-sync.lock
    ├── history.jsonl
    └── history-trim                          # was .history-trim
```

Decisions baked in:

- **One `.internal/` bucket instead of per-file dot-prefixes.** The rule: if
  shed ever prints a path for the user or an agent to visit, it's top-level
  (`repos/`, `workspaces/`, `logs/`); everything else lives under
  `.internal/`. This keeps `ls ~/.shed` showing exactly the two-concept
  model (plus logs), matters for agents who `ls` the parent of a repo path
  they were handed, and replaces the accumulating `.sync-errors` /
  `.sessions-pending` / `.bg-sync.lock` dot-convention with one rule — the
  moved entries lose their dots since the bucket already hides them.
  Named `.internal` (singular) after the Go convention, carrying the right
  meaning: works fine, not yours to depend on. `logs/` stays out because it
  exists *for* the user to look at when something's wrong.
- **Mirrors live under `.internal/mirrors/`**, keyed by URL-derived
  `host/owner/repo` with a `.git` suffix — the server-side convention for
  bare repos, visually unmistakable, and it separates "one per upstream"
  from "one per ref". This completes the hiding: mirrors are absent from
  config, from everyday vocabulary, and now from the visible tree. The
  mild cost is absolute mirror paths baked into each repo's `.git` pointer
  file, invisible in practice.
- **Bare repos have no `.git/` dir**, so `shed.lock` / `shed.meta` sit at the
  mirror's top level. The mirror's meta owns `LastSyncAt` / `LastError`
  (the mirror owns the network). A repo, being a worktree, needs almost no
  state of its own — its checkout state *is* its HEAD and its identity is
  in config; anything left (a per-repo error record, say) lives in the
  mirror's top-level `shed.meta`, keyed by repo — **not** in git's
  `worktrees/<id>/` admin dir, which `git worktree prune` deletes at
  exactly the moment a broken repo's record matters. First-clone failure
  records (`sync-errors/`) key by mirror.
- **Tag vs. branch is invisible in the path**, deliberately: the checkouts
  are structurally identical directories; the behavioral difference comes
  from what `track` resolves to in the mirror.
- **Workspaces keep their exact shape** — `<repo name>/<branch>` — with the
  possibly-`@`-suffixed repo name as the key. Two `fix-dag` workspaces off
  different Airflow checkouts never collide, and the path shows which version
  the work is based on.
- A repo remains exactly one leaf directory whose relative path *is* its
  name, so `PruneEmptyDirs` and orphan-dir detection stay trivial.

## Flows

### Sync

```
per mirror (network, exclusive lock on mirror):
  0. repair pass: re-detach any repo worktree whose HEAD is attached.
     (`git checkout main` inside a chmod-locked repo SUCCEEDS — it writes
      only to the mirror's admin area — and one attached branch anywhere
      makes the fetch below fail WHOLESALE, zero refs updated (verified).
      The most natural git command an agent can run breaks the detached
      invariant, so sync repairs it before every fetch.)
  1. git fetch '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*' \
       --prune --prune-tags                     ← the only network step
  2. refresh HEAD symref to upstream default branch
     (bare repos don't track it automatically; git ls-remote --symref)
  3. write shed.meta
  — release the exclusive lock here: holding it across the per-repo
    checkouts would block workspace creation (shared lock) for the whole
    span, minutes on a large monorepo —

per derived repo (local, deterministic, retryable; exclusive lock per repo):
  4. skip if HEAD already equals the track tip — the common case, and it
     avoids two full-tree chmod walks per repo per sync
  5. remove a stale index.lock if present (kill -9 mid-checkout is
     routine; git never cleans it up and the repo wedges permanently)
  6. unlock tree → git checkout --detach <track, BY REF NAME> → lock tree
     (a worktree reads the mirror's refs directly: no fetch, no objects
      move; creating a repo from zero is git worktree add --detach followed
      by a named checkout --detach — worktree add writes an empty reflog
      entry, so without the follow-up the status label is wrong until
      first sync)
```

The network call is no longer in the middle of a mutated working tree
(today's unlock → network fetch → checkout → relock in `syncOne`). A mirror
fetch is effectively atomic at the ref level; everything after it is local
and rebuildable offline. Two Airflow checkouts cost one fetch.

#### What a branch-tracked repo looks like from inside

Detached HEAD raised a UX worry: a user who knows the repo "tracks main"
cd's in and sees a bare SHA. The affordances mostly answer it, given three
rules (all verified on git 2.43 — the naive setup instead shows `Not
currently on any branch.`):

- the mirror sets `core.logAllRefUpdates=true` (bare repos default it off,
  so worktrees get no HEAD reflog and git can't name the detach point)
- repo creation follows `worktree add --detach` with a named
  `checkout --detach <track>` (worktree add writes an empty reflog entry)
- sync always detaches **by ref name, never raw SHA**

Then:

- `git status` → `HEAD detached at refs/heads/main` (2.43 prints the full
  ref form)
- `git log -1` → tip commit decorated `(HEAD, main)` — a worktree shares
  the mirror's refs, and the mirror's `main` is a real local branch at
  that commit
- shell prompts are the one uncontrolled surface: branch-aware prompts
  typically render detached HEAD as a short SHA. Acceptable — the first
  orienting command a confused user runs answers in the word "main", and
  `shed ls` should surface TRACK/SYNCED per repo as the authoritative
  answer.

Two rejected alternatives for non-detached repos (both trade the design's
sync properties for a prompt label):

- **Shed-owned per-repo branches** (`shed/<name>`, force-moved each sync,
  checked out non-detached — upstream never fetches into that name, so no
  fetch refusal). Rejected: ref-namespace pollution, `checkout -B`
  choreography every sync, per-repo branch names to dodge the
  one-checkout-per-branch rule, and the visible label (`shed/airflow`)
  explains no more than `detached at main`.
- **Pull-through-worktree**: check out the real branch, exclude
  checked-out branches from the mirror fetch (dynamic negative refspecs),
  and `git pull` inside each worktree — allowed, since pull moves the ref
  and working tree together. Rejected: pull has no local source to merge
  from (the mirror has no separate remote-tracking refs), so each
  branch-tracked repo fetches the network itself — breaking one-fetch-
  per-upstream, fragmenting ref consistency across per-repo fetch times,
  and re-interleaving network with tree mutation. Plus force-push needs a
  reset path and tag repos stay detached anyway (two modes). Nothing is
  saved: the tree unlock and the per-repo update step exist either way.

### Workspace creation

```
1. optional best-effort mirror fetch (same warn-and-proceed-if-stale
   fallback as today; hard-fail only if no mirror exists at all)
2. git clone --branch <branch> --config gc.auto=0 [--config k=v ...] \
     -- <mirror-path> <dest>
   (local: objects hardlinked, no --reference, no alternates)
3. git remote set-url origin <upstream-url>
4. new-branch case: git checkout -b <name>, as today
```

Step 3's failure mode is subtle and matters: a crash between clone and
`set-url` leaves a workspace whose `origin` is the mirror path — and a
push into a bare mirror *succeeds*, after which the next `--prune` fetch
silently deletes the pushed ref (verified). Two defenses: the mirror's
pre-receive hook rejects all pushes outright (see mirror creation spec),
and `workspace new` hitting an already-existing directory validates its
origin URL and repairs or replaces rather than trusting it. Failure
cleanup at any step remains `rm -rf <dest>`. The workspace's
remote-tracking refs were populated from the mirror and are semantically
upstream's refs; the first real `git fetch` reconciles against the true
remote. Clone failures are retried once — cheap insurance against losing
a race with a repack (see the gc section).

**LFS.** The mirror is bare and never smudges, so it holds pointer files
and no LFS objects; a workspace clone of an LFS repo would fail *at
checkout*, before `set-url` runs — git-lfs can neither find objects
locally nor derive an endpoint from a filesystem-path origin. Workspace
creation therefore clones with `GIT_LFS_SKIP_SMUDGE=1` (as the store does
today) and, after retargeting origin, runs `git lfs pull` when the repo
uses LFS. That reintroduces the network for LFS content specifically;
offline creation of an LFS workspace yields pointer files and says so.

One freshness note: the best-effort fetch in step 1 updates the mirror but
not sibling repo trees, so a just-created workspace can be newer than
`~/.shed/repos/<name>` until the next sync. Accepted — repos advance on
sync, workspaces on creation.

**Workspaces derive from the mirror, never from a repo.** The tiers are
siblings, not a chain: a repo is a worktree — no object store of its own,
one detached ref — so it cannot serve as a clone source, and routing user
work through disposable plumbing would be wrong even if it could. The repo
named on the CLI is a resolution and defaults source only — it selects the
mirror, supplies the base-branch default, and seeds the per-repo `Git`
config.

**Base-branch default:** a workspace created via a tracked repo
(`shed workspace new airflow@v2-7-stable my-fix`) bases on that repo's
`track`, not the upstream default — an agent working against the 2.7 checkout
wants to branch off 2.7. This falls out of the data model for free.

Offline creation of a branch that exists upstream but not yet in the mirror
fails with a clear error; acceptable.

### Repo removal, zombies, and empty upstreams

- Removal is **unlock tree → `git worktree remove`**, in that order.
  Against a chmod-locked tree, `worktree remove` deregisters the worktree
  *first* and then fails to delete the files (verified), leaving a
  zombie: a read-only dir with a dangling `.git` pointer — no longer a
  git repo, invisible to `git worktree list` and `worktree prune`, yet
  present to a naive `stat` existence check.
- Repo validity is therefore not "directory exists" but "`.git` pointer
  resolves to a live admin dir". Sync repairs invalid repos mechanically:
  chmod +w, `rm -rf`, re-add (verified clean on git 2.43 even without an
  intervening `worktree prune`; older gits need one — set a version
  floor).
- **Empty upstream** (no commits): `worktree add --detach` fails
  (`invalid reference: HEAD`) — there is nothing to check out. The repo
  entry holds an "empty" state with no directory; sync reports it as such
  rather than as an error, and the worktree materializes on the first
  sync after upstream gains commits. (Today's code has the analogous
  `RemoteHEADResolves` path.)

### gc: owned by shed, run in prune

`gc.auto=0` is pinned on every tier. That is not turning gc off — it is
transferring the responsibility for running it from git's per-repo
heuristics to shed, which knows the sharing topology. An auto-gc firing at
a moment of git's choosing is exactly what the topology can't tolerate:
inside a workspace it would rewrite the hardlinked packs and privatize the
entire shared base (a ~20 GB disk event on a big monorepo — not corruption,
but a shock); inside the mirror it would run outside shed's lock
discipline. So shed runs gc itself.

**It runs in `shed prune`, not sync.** Sync is frequent, fast, and should
stay network-focused; prune is the explicit, comparatively rare "reclaim
disk" moment, which the user is already watching. Prune's ordering is
deliberate:

1. remove landed/aged workspaces (as today)
2. `git gc` each mirror, under its exclusive lock
3. `git worktree prune` each mirror (clears admin records of removed repos)
4. remove mirrors no config entry references — nothing else ever deletes a
   mirror, and a hidden 20 GB bare repo must not outlive its last repo

Removing workspaces first matters: a mirror repack writes new inodes, and
surviving hardlinked workspaces keep the old packs alive (still one shared
copy *among themselves*) — repacking after removal minimizes that
un-shared remainder. No chmod dance is needed anywhere: gc writes only the
mirror's own object/ref store, never a repo's locked working tree.

Per tier:

- **Mirror** — the only tier with garbage to collect. Worktree HEADs (the
  repos) are gc reachability roots, so collection can never prune objects a
  repo's checkout needs — *reachability*-safe by construction (verified:
  `gc --prune=now --aggressive` after an upstream force-push left the
  worktree's detached commit intact and the tree usable). A conservative
  `gc.pruneExpire` remains cheap insurance. Scheduling, though, binds only
  shed: an agent running explicit `git gc` *inside a repo checkout*
  repacks the shared mirror store outside every shed lock (verified —
  `gc.auto=0` does not stop explicit gc). Hence: the agent guide states
  that repos are read-only including their git plumbing, and workspace
  creation retries a failed clone once as insurance against a lost race
  with a rogue repack.
- **Repos** — no object store of their own; nothing to collect, ever.
- **Workspaces** — shed never gc's them, and `gc.auto=0` (seeded at
  creation) means git doesn't either. A long-lived workspace therefore
  drifts: loose objects from agent commits accumulate, and a mirror repack
  un-shares its base. Accepted deliberately — workspaces are ephemeral, the
  bloat is temporary by definition, and prune deletes them wholesale.

The underlying asymmetry that shaped all of this: **clone hardlinks, fetch
copies.** A local-path clone hardlinks pack files (workspace creation is
~free even at 20 GB), but fetch always writes new private packs — which is
why long-lived advancing checkouts must not be fetching clones. Repos avoid
fetching entirely: as worktrees they read the mirror's refs directly, and a
tag-pinned repo touches nothing at all.

Sizing intuition, large monorepo: 20 GB of objects, ~100 MB/week of packed
churn, three advancing repo checkouts. The base is stored once (mirror),
plus at most one aging shared copy across surviving workspaces after a
repack; repos add zero, permanently. Hardlink-cloned repos would instead
grow ~5 GB/year per advancing checkout.

## Alternatives considered for the repo tier

Three mechanisms were weighed for the long-lived read-only checkouts. The
design initially chose (2) and was revised to (3) when the gc analysis was
worked through.

1. **Hardlink clones (like workspaces).** Zero cost at clone time, but
   *fetch copies*: an advancing checkout accumulates the full upstream
   churn privately, forever (~5 GB/year per checkout on a busy monorepo),
   plus one pack per fetch. Fine for ephemeral workspaces, wrong for
   permanent repos. (`.keep`-marking the base packs was considered to make
   this safe under gc; rejected as hand-reimplementing the dependency
   tracking worktrees give natively — which packs to mark, when removal is
   safe.)
2. **`--shared` clones (alternates to the mirror).** Zero duplication and a
   real `.git` dir — but the dependency is *invisible to git*: the mirror's
   gc doesn't know the repo exists, so a force-pushed upstream branch plus
   prune could remove objects a repo's detached checkout still references.
   Living with that means defensive gc scheduling, broken-repo detection,
   and a re-derive path — shed hand-managing what git can't see.
3. **Detached worktrees of the mirror (chosen).** Same zero duplication,
   but the dependency is *declared*: worktree HEADs are gc reachability
   roots, so the mirror can be collected at any time with no corruption
   race and no bookkeeping. The costs are real but small and entirely
   shed-internal: `.git` is a pointer file (per-repo state shrinks to
   nearly nothing; leftovers live in the mirror's `worktrees/<id>/` admin
   dir), per-repo git config needs `extensions.worktreeConfig`, removal is
   `git worktree remove` rather than bare `rm -rf`, and worktree operations
   serialize on the mirror's exclusive lock.

Two worktree limitations are non-issues here, one by design and one by
invariant:

- **Detached HEADs are mandatory, not a preference.** Git refuses to fetch
  into a branch that is checked out in *any* worktree — a repo worktree
  sitting on `main` would make the mirror's own `+refs/heads/*` fetch fail
  for `main`. Detached checkouts keep every branch fetchable; sync moves
  the repo afterward with `checkout --detach`.
- **Same-branch-twice never arises.** Two repos of one upstream at the same
  `track` would be identical read-only trees — pure duplication with no
  use. The derived `@track` naming already collides them at config time,
  and `config.Validate` rejects duplicate `(url, track)` pairs even under
  explicit `name` overrides. The constraint costs nothing because the
  thing it forbids is worthless.

The README's "why not `git worktree`" rationale is re-scoped, not reversed:
it argued from user work, pushing, and independent teardown — all true of
workspaces, none true of repos.

## No migration

shed is unreleased; the new layout lands as *the* layout in one change.
No old-store detection, no dual-layout code, no alternates dissociation.
Existing `~/.shed/repos/` contents from the old scheme are simply invalid —
blow away and re-sync.

This also frees internal naming: `pkg/repostore` splits/renames into a mirror
package and a repo-checkout package, path helpers rename accordingly, and the
README's design-rationale sections ("Why a read-only store…", "Why
`git clone --reference`, not `git worktree`") are rewritten for the mirror
model — the `--reference`/alternates justification no longer describes how it
works. The README's worktree rejection is re-scoped rather than reversed: it
still holds for workspaces (user work, independence, plain `rm -rf`
teardown), while repos — shed-owned read-only checkouts — are now exactly
worktrees.

## Verified behaviors (red-team pass, git 2.43)

A red-team review empirically tested the load-bearing git claims. Sound:
detached worktree HEADs survive `gc --prune=now --aggressive` after an
upstream force-push; `worktree add --detach` works from bare repos for
branches and annotated tags; `clone --branch` from a bare local mirror
works for branches (attached HEAD) and tags (detached; `checkout -b`
after works); local clones hardlink packs (pack st_nlink observed 1→3);
`extensions.worktreeConfig` isolates per-repo config once `core.bare` is
relocated; workspace clones get `refs/remotes/origin/HEAD`, so
landed-work prune logic survives `remote set-url`; short-name
branch-over-tag precedence in `clone --branch` matches the doc.

The same pass forced the spec above: the pre-fetch detach-repair pass,
forced tag refspec + `--prune-tags`, `core.bare` relocation, the
pre-receive hook, `core.logAllRefUpdates`, LFS handling, unlock-before-
remove, zombie/validity checks, stale index.lock cleanup, empty-upstream
state, mirror identity canonicalization, and mirror end-of-life in prune.

## Open questions / remaining notes

1. **Hardlink behavior across filesystems** — local clone falls back to
   copying when `~/.shed` spans devices; still correct, just slower/bigger.
   Doc note only.
2. **Mirror HEAD refresh cadence** — `ls-remote --symref` is an extra
   network round-trip per sync; confirm it's cheap enough to do every sync
   or gate it (e.g. only when the fetch reports ref changes).
3. **Lock ordering** — shared (workspace creation) vs exclusive (sync
   phases, worktree ops, gc). `workspace new` makes two separate
   acquisitions (fetch, then clone) — never an in-place flock upgrade,
   which deadlocks when two holders attempt it simultaneously.
4. **chmod walk with a `.git` file** — today's `chmodTree` returns
   `filepath.SkipDir` at `.git`; returned from a non-dir entry, `SkipDir`
   skips the rest of the *parent directory*, silently leaving most of the
   repo root writable. The rewrite must return nil for the pointer file.
5. **Case-insensitive filesystems** — a mirror materializes all upstream
   branches as loose refs; `Foo`/`foo` branch pairs collide on APFS/NTFS
   (same exposure as today's remote-tracking refs). `@`-names differing
   only by case likewise collide as directories.
6. **Disk reporting** — a repo dir is now just a working tree; the real
   bytes live under `.internal/mirrors/`. `shed ls`/sync output needs a
   story (per-mirror size + tree-only per repo) or a 20 GB install
   reports megabytes.

## Implementation sequence

1. **Paths + config.** New `.internal/` root and `mirrors/` path helpers;
   relocate existing plumbing paths (`sync-errors`, `sessions-pending`,
   `bg-sync.lock`, history files) under it; `Track` field on `Repo`; name
   derivation with `@` + sanitization; `Validate` collision check (name
   uniqueness and sanitized-path uniqueness).
2. **Mirror package.** Bare clone into temp-then-rename with explicit
   refspecs (heads + forced tags), `--prune --prune-tags` fetch,
   HEAD-symref refresh, creation-time config (`extensions.worktreeConfig`
   + `core.bare` relocation, `core.logAllRefUpdates`, `gc.auto=0`),
   pre-receive reject hook, canonical URL identity + transport-conflict
   warning, lock/meta sidecars at top level, sync-errors keyed by mirror.
3. **Repo worktree package.** Create = `git worktree add --detach` + named
   `checkout --detach` → tree lock; update = pre-check ref exists, clear
   stale index.lock, skip-if-unchanged, unlock → `checkout --detach` by
   ref name → lock; removal = unlock → `git worktree remove` (+ prune);
   validity = `.git` pointer resolves, with chmod+w/rm/re-add repair;
   empty-upstream state; per-repo git config via worktreeConfig.
4. **Sync rewrite.** `syncOne` becomes detach-repair → fetch mirror →
   update worktrees, releasing the exclusive lock between phases; one
   fetch per mirror across N repos; meta moves to the mirror.
5. **Workspace creation rewrite.** Local clone from mirror (`gc.auto=0`
   and `GIT_LFS_SKIP_SMUDGE=1`, retry once) + `remote set-url` +
   `git lfs pull` when applicable; short-name normalization for
   `--branch`; origin validation when the directory already exists; drop
   `--reference`; base-branch defaulting from the source repo's `track`;
   keep the best-effort-sync-first / stale-fallback behavior.
6. **Prune gains gc.** After workspace removal: `git gc` per mirror under
   its exclusive lock, then `git worktree prune`, then orphan-mirror
   removal; orphan repo-dir detection (dirs with no config entry).
7. **CLI + resolution.** `@`-suffixed names through `Resolve`, `shed add`
   growing a way to specify `track`, `shed sync` output that mentions
   mirrors.
8. **Docs.** README model rewrite (repos you read, workspaces you write,
   mirrors as plumbing); embedded agent guide unchanged in vocabulary.
