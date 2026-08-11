# shed welcome tour — instructions for the agent

Provide a tour of shed.

## Pacing (critical)

Text you write between tool calls is **not** displayed to the user. Only your
opening message, AskUserQuestion prompts, and your final message render.

**Never narrate a step between tool calls** — the user will not see it.

Before each major step, pause with **AskUserQuestion**: state in the question
text exactly which command you are about to run and why, and offer a "Continue"
option alongside one alternative. Save all remaining commentary for the final
recap message.

If a step fails (`gh` not authenticated, no network, permission errors you
didn't expect), say so in the next user-visible message, explain what it would
have shown, and offer a fallback — don't stall the tour on a broken prerequisite.

Run real commands. Never fabricate output.

Open with a short welcome: the tour takes a few minutes, they can stop or ask
anything at any point. Then go straight to checkpoint 1.

---

## Checkpoint 1 — Add a repo (`shed add`)

Ask the user whether they'd like to track:

- **A GitHub owner** (such as themselves) — recommended if they have at least
  one repo on GitHub. `shed add <owner>` discovers all their repos and keeps
  discovering new ones on every sync.
- **A specific repo** (e.g. `psf/requests`) — recommended if they have no repos
  on GitHub.

Fold this choice and the `shed add` explanation into the AskUserQuestion.
Include a "Continue" option and one alternative (e.g. skip adding for now, or
pick the other add mode).

If helpful, use `gh api user --jq .login` to suggest their personal owner when
`gh` is authenticated.

When they continue, run the agreed `shed add` command, then `shed ls` to show
the result.

---

## Checkpoint 2 — Prove the catalog is read-only

AskUserQuestion: you are about to attempt a write (e.g. `touch` or append to a
file) under `~/.shed/repos/…` to demonstrate that the catalog is read-only —
offer "Continue" and one alternative.

Run the attempt. Show the actual permission error.

The reason it is read-only: the repo catalog stays fresh and pristine. The code
is always up to date and can't be clobbered. Agents read here; all editing
happens in workspaces.

---

## Checkpoint 3 — First workspace (`shed workspace new`)

AskUserQuestion: you are about to run `shed workspace new <owner>/<repo>
<name>` to create a writable clone for editing — offer "Continue" and one
alternative (e.g. pick a different repo or workspace name).

Run `shed workspace new`. Briefly note in the checkpoint reply or final recap
that shed synced first, printed a path, and this is a normal writable clone
whose origin is the real upstream.

Demonstrate using shed from the workspace: explore the repo, or make a small
illustrative change if appropriate — but **do not commit junk or open a PR**.

Emphasize (in the final recap) that **`shed workspace new` is typically run by
the agent, not the user.**

---

## Checkpoint 4 — Second workspace (multiple workspaces)

AskUserQuestion: you are about to run `shed workspace new` again (same or
another repo, different name) to show that one session can manage multiple
isolated workspaces — offer "Continue" and one alternative.

Create the second workspace. **Do not open a PR.** Instead, explain in the
final recap what you could do from there: edit independently, commit, push,
open a PR — each workspace is its own clone on its own branch, so they don't
collide.

---

## Final message — recap

In one message, recap what the tour showed:

1. **Catalog** — repos added with `shed add`, read-only under `~/.shed/repos/…`,
   kept current automatically.
2. **Read-only demo** — why the catalog stays pristine.
3. **Workspaces** — writable clones via `shed workspace new`; agents create
   these, not the user.
4. **Multiple workspaces** — isolated clones for parallel work; from there you'd
   commit, push, and open PRs as usual.

Also mention:

- **In practice**, shed interactions are more background with less exposition.
  This tour was verbose for illustration.
- **`shed workspace new` is typically run by the agent**, not the user.
- **Next steps**: `shed ls` to see everything, `shed help` for more, `shed
  workspace rm <name>` or `shed prune` to tidy up when done.
