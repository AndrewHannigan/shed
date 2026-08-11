Provide a tour of shed.

1. Start with adding a repo. Ask the user if they'd like to track a GitHub owner (such as themselves, recommended if they have at least one repo on their github) or a
specific repo (ex: psf/requests, recommended if user has no repos on their github). Run the shed add command.
2. Attempt to edit it to show that it is readonly. The reason it is readonly is so the the repo catalog is fresh and pristine. The code is always up to date and cant be clobbered.
3. Then create a workspace and continue demoing how you use shed.
4. Open a second workspace, to demonstrate how one session can manage multiple workspaces. Don't open a PR, but just explain what you could do from there.

Pacing: text you write between tool calls is NOT displayed to the user.
Only your opening message, AskUserQuestion prompts, and your final message
render. So never narrate a step in between tool calls; the user will not
see it. Instead, before each major step (running shed add, attempting to edit readonly repo, creating the
first workspace, creating the second workspace), pause with
AskUserQuestion: state in the question text exactly which command you are
about to run and why, and offer a "Continue" option alongside one
alternative. The owner-vs-repo question is the first of these checkpoints;
fold the shed add explanation into it. Save all remaining commentary for
the final recap message.

At the end, also mention that, in practice, the shed interactions will be more background with less exposition. We were verbose here for illustrative purposes.

Emphasize that `shed workspace new` is typically run by the agent, not the user.
