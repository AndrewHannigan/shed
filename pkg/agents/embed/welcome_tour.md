# shed welcome tour — instructions for the agent

You are giving the user a short, live tour of `shed`. This document is the
script. The user is watching, and **this is a conversation, not a lecture** —
your job is to orient them, get their **real repos** into shed, and then show
shed working on one of those repos. No dummy repos, no toy examples: when the
tour ends, the user should be set up and have seen shed do something real —
ideally with a genuine change pushed up as a PR.

There are three steps (the first is plain talk, no commands) and a wrap-up.
Don't rush through them.

## The most important rule: pause and wait

After each step, **stop and hand the conversation back to the user.** Briefly ask
something like *"Make sense? Any questions, or should I keep going?"* — then
**actually wait for their reply.** Do not run the next step until they answer.

If they ask a question, answer it, then ask again whether they're ready to
continue. The tour only moves forward when the user says so.

## Assume zero context

Treat the user as a **complete newcomer** unless they show otherwise. They may
not know what shed is, why it's on their machine, or that their coding agents —
not they themselves — are its main users. Don't lean on a term the tour hasn't
earned yet: "store", "workspace", and "sync" mean nothing until you've shown
them.

If at any point the user says they're lost, confused, or asks "what even is
this?" — **do not repeat your last explanation in different words.** Back up
and change tactics:

- Ground shed in concrete facts they can see: `which shed` (it's an ordinary
  CLI installed on their machine), `shed ls` (here's what it's managing right
  now, including any workspaces past agent sessions created).
- Re-explain from the **problem** (the mess of agents sharing ad-hoc clones),
  not from shed's features.
- Then ask what's still unclear, and only resume the tour when they say
  they're ready.

## A few more rules

- **Run real commands, one at a time.** Show the command, run it, say in a
  sentence or two what happened. Never fabricate output.
- **Keep narration tight.** One or two plain-language points per step — not every
  detail. Deeper mechanics (the shared per-upstream mirror, hardlinked objects,
  the exact `chmod`) are **only worth explaining if the user asks.**
- **This is their real code.** Every edit you make in step 3 must be one the
  user asked for or approved first — no throwaway "test edit" commits, no junk
  changes to a real repo, ever.
- **If a step fails** (`gh` not authenticated, no network, a push rejected), say
  so plainly, explain what it would have shown, and fall back where the script
  offers a fallback — don't stall the tour on a broken prerequisite.

Open by saying the tour takes a few minutes and they can stop or ask anything
at any point. Then begin with step 1 — the framing itself is step 1's job.

---

## Step 1 — What shed is, and why it exists (no commands yet)

Start from the problem, in plain language, roughly:

> "When a coding agent works on your code, the default approach is 'clone the
> repo somewhere and start editing.' That gets messy fast: clones scattered
> around your disk go stale, an agent leaves one stuck on a half-finished
> branch, and two agents editing the same clone trample each other's changes."

Then shed's answer, in two parts:

- **A library** — one pristine, always-current, **read-only** copy of each repo
  you care about, kept under `~/.shed/repos/`. A reference shelf: always safe
  to read, impossible to scribble on.
- **Workspaces** — when an agent needs to *change* something, it asks shed for
  a fresh, writable clone of its own. Each task gets its own; they can't see
  each other's edits; they're removed when the work lands.

Make one thing explicit, because it surprises newcomers: **shed is a tool for
your agents more than for you.** Agents open the workspaces and do the editing;
you mostly just run `shed add` to put a repo on the shelf and `shed ls` to see
what's there.

Optionally run `shed ls` to ground this in their actual machine — "here's your
library right now." (An empty library is a fine answer; it leads straight into
step 2.)

**→ Pause. Ask if that framing makes sense, and wait before continuing.**

## Step 2 — Put your real repos on the shelf

Don't demo on a dummy repo — build the user's actual library, so the tour
leaves them set up rather than just entertained. Ask:

> "Is there a GitHub org or owner you usually work out of? Or, failing that, a
> repo or two you touch most often?"

Guidance for that conversation:

- **Prefer a whole owner (user or org) over a single repo.** `shed add <owner>`
  tracks the owner: it discovers their repos automatically and keeps
  discovering new ones on every sync — one command, whole library. If the user
  works inside an org, that org is the best first add.
- **Suggest their own personal owner as a starting point.** If `gh` is
  authenticated you can look up their login with `gh api user --jq .login` and
  offer it: "want me to add `<login>`, so your personal repos are all on the
  shelf?"
- **A single repo is a fine fallback.** If they'd rather start small — or owner
  tracking isn't possible right now (it needs `gh` installed and
  authenticated) — add individual repos instead: `shed add <owner>/<repo>`.

Then run what you agreed on:

```
shed add <owner>          # a whole user or org
shed add <owner>/<repo>   # or a single repo
```

and show the result:

```
shed ls
```

Say briefly what happened: their repos were fetched into the **read-only**
library under `~/.shed/repos/…`, and — if an owner was added — sync will pick
up that owner's new repos automatically from now on.

Now prove the shelf really is read-only, quickly, in one of *their* repos: try
to `touch` or append to a file under `~/.shed/repos/…`. It **fails with a
permission error** — show the user the actual error. One or two sentences on
why: the store stays a pristine, always-current baseline that no agent can
clobber or leave stranded on a half-finished branch. All editing happens
somewhere else — which is step 3.

**→ Pause. Ask if they have questions, and wait before continuing.**

## Step 3 — A real change, in a real workspace

> "Now let's actually use it. When your agent needs to change something, it
> asks shed for a workspace: an isolated, writable clone."

First, pick the work **with the user**:

- Ask which of the repos they just added they'd like to make a change in.
- Ask whether there's a small, real change they've been meaning to make — a
  typo, a README fix, a nagging TODO, a tiny cleanup. If nothing comes to
  mind, take a quick look at the repo and **propose** something small and
  genuinely useful — and get their OK before touching anything.

Then open the workspace, named after the change like a branch:

```
shed workspace new <owner>/<repo> <change-name>
```

Say, briefly:
- It synced the repo first, so the workspace starts from the **latest** code.
- It printed the **path** to a writable clone. `cd` there, make the agreed
  change, and commit it with a real commit message.

Land the isolation point in a sentence while you work: this workspace is its
own clone on its own branch — the user could open five more on the same repo
and none would see each other's edits. Workspaces are **isolated**, which is
exactly what lets several agents (or one agent juggling tasks) work the same
repo at once without colliding.

Then offer to ship it:

> "Want me to push this up and open a PR?"

- **If yes:** run `git push -u origin <change-name>` and open the pull request
  (e.g. with `gh pr create`). The workspace is an ordinary clone whose origin
  is the real upstream, so this is just the normal flow — and the user walks
  away from the tour with a real PR up.
- **If no:** that's fine — say in a line that pushing works like any other
  clone, whenever they're ready.

**→ Pause. Ask if they have questions, and wait before continuing.**

## Wrap-up — recap, what's next, tidy up

Recap what the tour did, briefly — and note that none of it was throwaway:
- **The problem** — agents sharing ad-hoc clones step on each other.
- **Their library is live** — their real repos are on the shelf, read-only and
  kept current automatically.
- **A real change happened** — made in an isolated workspace, off the latest
  code, and (if they said yes) pushed up as a PR.

Then mention — in a line or two each, no need to run them — where to go next:
- **Grow the library:** more `shed add`, any time — repos or whole owners.
- **Let the agents take it from here:** their coding agents already know about
  shed and will open workspaces themselves when asked to edit these repos.
- **Tidy up later:** `shed prune` removes workspaces whose work has already
  landed — so once the tour's PR merges, prune cleans it up.

Tidying up is different from a dummy-repo demo: **the library stays** — it *is*
the setup, that's the point. For the workspace: if a PR went up, suggest
keeping the workspace until the PR lands (then `shed prune`). If the change
was never pushed, ask whether they want to keep it or remove it with
`shed workspace rm <change-name>` — leave that choice to them.

Close by pointing them at `shed help` for anything they want to dig into.
