package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/history"
)

// recordedCommands are the command paths (relative to the root command) whose
// successful runs are appended to the history log: the "working" commands that
// change the library or workspaces. Everything else — read-only queries (ls,
// status, workspace ls/path), `sync`, the `history` command itself, the hidden
// internal commands, and a bare `shed` — is intentionally absent.
var recordedCommands = map[string]bool{
	"add":           true,
	"rm":            true,
	"repo add":      true,
	"repo rm":       true,
	"owner add":     true,
	"owner rm":      true,
	"prune":         true,
	"init":          true,
	"workspace new":     true,
	"workspace from-pr": true,
	"workspace rm":      true,
}

// shouldRecord reports whether a command's successful run should be logged.
func shouldRecord(cmd *cobra.Command) bool {
	key := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	return recordedCommands[key]
}

// recordCommand appends the current invocation to the history log when the
// command is one we track. Best-effort and silent: logging must never change a
// command's behavior or exit status, so all errors are swallowed.
func recordCommand(cmd *cobra.Command) {
	if shouldRecord(cmd) {
		_ = history.Record(os.Args[1:])
	}
}

func newHistoryCmd() *cobra.Command {
	var limit int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show recent shed commands",
		Long: `history prints the most recent shed commands that changed the
library or workspaces (add, rm, prune, init, workspace new/from-pr/rm), newest
last. Read-only queries and background syncs are not recorded.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistory(limit, jsonOut)
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "how many recent commands to show")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a human-readable table")
	return cmd
}

func runHistory(limit int, jsonOut bool) error {
	events, err := history.Recent(limit)
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(events)
	}
	if len(events) == 0 {
		fmt.Println("(no history recorded yet)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tCOMMAND")
	for _, ev := range events {
		fmt.Fprintf(w, "%s\t%s\n", eventTime(ev), eventCommand(ev))
	}
	return w.Flush()
}

// eventTime / eventCommand render one event for display. The time is a
// fixed-width local timestamp (so columns align without a tabwriter), and the
// command is reconstructed from the raw args exactly as the user typed it.
func eventTime(ev history.Event) string {
	return ev.Time.Local().Format("2006-01-02 15:04")
}

func eventCommand(ev history.Event) string {
	return "shed " + strings.Join(ev.Args, " ")
}
