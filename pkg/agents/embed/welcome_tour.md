Provide a tour of shed.

Explain up front that shed is a tool that helps agents manage git repos and workspaces. Mention that you can see usage instructions already in your session context, which means the user has already run `shed init` (if you can't see this then flag to the user). 

1. Start with adding a repo. Ask the user if they'd like to track a GitHub owner (such as themselves, recommended if they have at least one repo on their GitHub) or a specific repo (e.g. psf/requests, recommended if the user has no repos on their GitHub). Run the `shed add` command.
2. Attempt to edit it to show that it is read-only. The reason it is read-only is so the repo catalog is fresh and pristine. The code is always up to date and can't be clobbered.
3. Then create a workspace and continue demoing how you use shed.
4. Open a second workspace to demonstrate how one session can manage multiple workspaces. Don't open a PR, but just explain what you could do from there.

Pacing: text you write between tool calls is NOT displayed to the user.
Only your opening message, AskUserQuestion prompts, and your final message
render.

So never narrate a step in between tool calls; the user will not see it.
Instead, before each major step (running `shed add`, attempting to edit a
read-only repo, creating the first workspace, creating the second workspace),
pause with AskUserQuestion: state in the question text exactly which command
you are about to run and why, and offer a "Continue" option alongside one
alternative. The owner-vs-repo question is the first of these checkpoints;
fold the `shed add` explanation into it. Save all remaining commentary for
the final recap message.

At the end, also mention that, in practice, shed interactions will be more background with less exposition. We were verbose here for illustrative purposes.

Emphasize that `shed workspace new` is typically run by the agent, not the
user.

Always call it the read-only catalog, not read-only mirror.
