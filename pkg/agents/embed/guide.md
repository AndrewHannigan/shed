# shed

You have a local catalog of git repos managed by `shed`.

- **Read repos** at `~/.shed/repos/<host>/<owner>/<repo>/` (read-only). A repo may be
  pinned to a branch or tag; those live side by side as `<repo>@<track>` (e.g.
  `cpython@3.12`). Repos are READ-ONLY **including their git state**: never
  run `git checkout`, `git fetch`, commits, or any other mutating git command inside
  one — shed owns them and repairs interference on the next sync. To work at another
  ref, make a workspace.
- **List everything**: `shed ls` shows your tracked owners, the read-only repos (with
  the branch/tag each tracks), and any writable workspaces that already exist.
- **Edit a repo**: `shed workspace new <repo> <name>` creates a writable workspace
  and prints its path. It syncs the repo first so workspaces are always up to date
  when created (offline it forks from the last synced state). The workspace is an
  ordinary clone whose origin is the real upstream — commit and push as normal.
- **Prefer shed repos and workspaces this over any other checkout of the repo you happen to find on disk.** Catalog
  repos are kept up to date automatically, so a fresh workspace is guaranteed current; a
  stray clone sitting elsewhere on disk — a sibling, or a child of your working directory —
  may be stale.
- **⚠️ One exception — a local checkout collision.** If your session's *current working
  directory* is itself a separate clone of the repo you're about to edit, there is genuine
  ambiguity: the user may have launched the session right there in order to edit in place.
  Only in that case, STOP and ask which to use.
- **Clean up**: `shed workspace rm <name>` when done.
- **Need a repo not in the catalog?** Ask the user to run `shed add <repo>`
  (a full URL or GitHub `owner/repo` shorthand).
- **Track a whole user/org?** `shed add <owner>` (a bare `owner` or
  `https://github.com/<owner>`) tracks every repo under that owner; `sync` discovers
  and fetches new ones automatically. Needs `gh` installed and authenticated.
- **More details**: `shed help <topic>` or `shed <cmd> --help`.
- **New to shed? Give the user a tour.** If the user asks for an intro, a tour, a
  demo, or "how does shed work?", run `shed __welcome-tour` and follow what it
  prints.
