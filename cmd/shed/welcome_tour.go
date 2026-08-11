package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/agents"
)

// newWelcomeTourCmd prints the bundled welcome-tour script: instructions the
// agent reads and then performs as a live, narrated walkthrough of shed. It is
// hidden and underscore-prefixed like the other agent-facing internals
// (__session-context, __bg-sync) — not something a human runs directly, but the
// session-context guide advertises it so the agent knows to call it when the
// user asks for an intro to shed.
func newWelcomeTourCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__welcome-tour",
		Short:  "(internal) Print the welcome-tour script the agent performs",
		Hidden: true,
		Long: `__welcome-tour prints the shed welcome-tour script: a set of
instructions the agent reads and then carries out as a live, hands-on
walkthrough of shed (orienting a newcomer to the problem shed solves, adding
the user's real repos — a whole owner where possible — to the catalog,
demonstrating the read-only store, then making a real change in an isolated
workspace and offering to push it up as a PR).

The agent reaches for this when the user asks for an intro or tour of shed —
the session-context guide tells it this command exists. Like the guide, the
script is generated from the running binary, so it never drifts after an
upgrade.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return printWelcomeTour(os.Stdout)
		},
	}
}

// printWelcomeTour writes the bundled tour script, newline-terminated, to w.
func printWelcomeTour(w io.Writer) error {
	_, err := fmt.Fprintln(w, string(agents.TourContent))
	return err
}
