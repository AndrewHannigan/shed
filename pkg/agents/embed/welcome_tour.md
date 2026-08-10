# shed welcome tour — instructions for the agent

You are giving the user a short, live tour of `shed`. This document is the
script. The user is watching, and **this is a conversation, not a lecture** —
your job is to orient them first, then show three things, one at a time, and
check in with the user after each one.

There are four short steps (the first is plain talk, no commands) and a
wrap-up. Don't rush through them.

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
- **If a step fails** (`gh` not authenticated, no network, a push rejected), say
  so plainly, explain what it would have shown, and continue.
- **Clean up at the end** and tell the user what you removed.

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
  each other's edits; they're cheap to clean up once the work lands.

Make one thing explicit, because it surprises newcomers: **shed is a tool for
your agents more than for you.** Agents open the workspaces and do the editing;
you mostly just run `shed add` to put a repo on the shelf and `shed ls` to see
what's there.

Optionally run `shed ls` to ground this in their actual machine — "here's your
library right now." (An empty library is a fine answer; it leads straight into
step 2.)

**→ Pause. Ask if that framing makes sense, and wait before continuing.**

## Step 2 — A library you can't accidentally break

> "Let's add a repo to the library."

Run:

```
shed add octocat/Hello-World
```

Then say, briefly:
- The repo is fetched right away and now lives in the **read-only** library
  under `~/.shed/repos/…` — a pristine copy that's always there and safe to read.

Now prove it's read-only: find a file in that library (e.g. the `README`) and
**try to edit it in place** — append a line or `touch` it. It **fails with a
permission error.** Show the user the actual error.

Then explain *why* this is the design, briefly: the library is locked down so
your agent can't clobber it — it stays a clean, always-current baseline, never
half-edited or stuck on some branch an agent forgot to leave. That's what makes
it safe for agents to read across many repos, and it means every change starts
from a known-good copy. To actually make changes, your agent doesn't touch the
library — it opens an isolated *workspace*, which is next. (Read-only isn't the
point on its own; it's what keeps the writable workspaces safe to spin up
freely.)

**→ Pause. Ask if they have questions, and wait before continuing.**

## Step 3 — Your first workspace

> "When your agent needs to change something, it asks shed for a workspace: an
> isolated, writable clone."

Run:

```
shed workspace new octocat/Hello-World tour-feature-a
```

Say, briefly:
- It synced the repo first, so the workspace starts from the **latest** code.
- It printed the **path** to a writable clone. `cd` there, make one small, obvious
  edit (e.g. add a line to the README), and commit it:

```
git add -A && git commit -m "Tour edit A"
```

**→ Pause. Ask if they have questions, and wait before continuing.**

## Step 4 — Two workspaces, no collisions (the payoff)

> "Here's the part that matters. Let me open a *second* workspace on the same repo
> and make a *different* change."

Run:

```
shed workspace new octocat/Hello-World tour-feature-b
```

`cd` into this one, make a **different** edit, and commit:

```
git add -A && git commit -m "Tour edit B"
```

Now show they're **isolated**: in workspace **b**, run `git log --oneline -2` —
it has *Tour edit B* but **not** *Tour edit A*. Pop back to **a** and show the
reverse.

Drive the point home in a sentence: two writable clones of the same repo, on
different branches, that can't see each other's changes — which is exactly what
lets multiple agents (or one agent juggling several tasks) work the same repo at
once without colliding.

**→ Pause. Ask if they have questions, and wait before continuing.**

## Wrap-up — recap, what's next, clean up

Recap what the tour showed, briefly:
- **The problem** — agents sharing ad-hoc clones step on each other.
- **Read-only library** — a pristine baseline that's impossible to clobber.
- **Isolated workspaces** — your agents edit many things at once, always off the
  latest code, without collisions.

Then mention — in a line or two each, no need to run them — where to go next:
- **Ship it:** from a workspace your agent would `git push -u origin tour-feature-a`
  and open a PR like normal; each workspace pushes independently, so you get clean
  separate PRs. (Against this public demo repo a push would be rejected — that's expected.)
- **Track a whole owner:** `shed add octocat` tracks an entire user/org and
  auto-discovers its repos.
- **Tidy up later:** `shed prune` removes workspaces whose work has already landed.

Now clean up what the tour created, and tell the user what you're removing:

```
shed workspace rm tour-feature-a --force
shed workspace rm tour-feature-b --force
```

Ask whether they want to keep `octocat/Hello-World` in their library or remove it
with `shed rm` — leave that choice to them. Close by pointing them at
`shed help` for anything they want to dig into.
