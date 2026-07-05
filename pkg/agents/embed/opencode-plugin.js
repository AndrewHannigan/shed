// shed:managed — installed by `shed init`. Safe to delete; shed
// will reinstall it on the next `init`, or run `shed init --uninstall` to remove.
//
// opencode has no SessionStart shell-command hook and no per-directory
// access allowlist, so shed integrates as a plugin that
// shells back to the `shed` binary. The plugin module body runs once when
// opencode loads it at startup — that is our "session start":
//
//   - kick off the background cache refresh (`shed __bg-sync`), and
//   - snapshot the guide (`shed __session-context --agent opencode`) to
//     inject into the model's system prompt via experimental.chat.system.transform.
//
// All logic lives in the binary, so this file never needs to change across
// shed upgrades.

export const ShedPlugin = async ({ $, directory }) => {
  // Non-blocking background refresh, like the SessionStart bg-sync hook the
  // other agents get. Errors are ignored so a stale/uninstalled binary can
  // never break the session.
  $`shed __bg-sync`.catch(() => {})

  // Snapshot the guide once at startup (matches how the other agents snapshot
  // the repo-list at hook time). Run in the project directory so the
  // cwd-collision warning resolves against the right repo.
  let guide = ""
  try {
    guide = (await $`shed __session-context --agent opencode`.cwd(directory).quiet().text()).trim()
  } catch {
    guide = ""
  }

  return {
    "experimental.chat.system.transform": async (_input, output) => {
      if (guide) output.system.push(guide)
    },

    // Pre-tool hook: when the model runs `shed workspace new` (or from-pr)
    // via the bash tool, hand shed the session id + command + cwd so it can
    // link the new workspace to this session (for `shed resume`). The id is on the first
    // arg, the command on the second (output.args.command). We pre-filter here
    // — only a workspace-new command shells out to shed — so shed is never
    // invoked on ordinary tool calls. Best-effort and non-blocking: errors are
    // swallowed so a tool call is never disrupted.
    "tool.execute.before": async (input, output) => {
      try {
        if (input.tool !== "bash") return
        const command = output && output.args && output.args.command
        if (!command) return
        if (command.indexOf("workspace new") === -1 && command.indexOf("ws new") === -1 &&
            command.indexOf("workspace from-pr") === -1 && command.indexOf("ws from-pr") === -1) return
        const payload = JSON.stringify({
          sessionID: input.sessionID,
          command,
          cwd: directory,
        })
        await $`shed __on-tool-call --agent opencode`.stdin(payload).quiet().catch(() => {})
      } catch {}
    },
  }
}
