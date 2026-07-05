package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/catalog"
	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

func newLsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List your shed: tracked owners, read-only repos, and writable workspaces",
		Long: `ls shows everything shed is managing for you, in three sections:

  Tracked Owners  whole GitHub users/orgs you track; sync auto-adds their repos
  Repos           read-only reference copies your agents read from
  Workspaces      isolated writable clones where agents make and push changes

A repo's "⚠ sync failing" marker means its last fetch failed, so its stored
copy is stale — run 'shed status <repo>' for the error and the fix.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoList(jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a human-readable table")
	return cmd
}

type repoRow struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	Track      string `json:"track,omitempty"`
	Source     string `json:"source,omitempty"`
	Path       string `json:"path,omitempty"`
	LastSyncAt any    `json:"last_sync_at"`
	LastError  string `json:"last_error,omitempty"`
}

type ownerRow struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	RepoCount int    `json:"repo_count"`
}

func runRepoList(jsonOut bool) error {
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	rows, owners, err := collectRepoList(c)
	if err != nil {
		return err
	}
	workspaces, err := collectWorkspaces(c)
	if err != nil {
		return err
	}
	if jsonOut {
		if workspaces == nil {
			workspaces = []workspace.Info{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Owners     []ownerRow       `json:"owners"`
			Repos      []repoRow        `json:"repos"`
			Workspaces []workspace.Info `json:"workspaces"`
		}{owners, rows, workspaces})
	}
	// Interactive `ls` shows the empty-workspaces hint so a newcomer who has
	// repos but no workspaces yet learns the concept exists and how to create one.
	return writeLibrary(os.Stdout, owners, rows, workspaces, true)
}

// runRepoOnlyList backs `shed repo ls`: the read-only repos only — the
// reference copies your agents read from — without the Owners section or the
// Workspaces section that top-level `shed ls` adds. Owners are listed by
// `shed ls`, and workspaces have their own `shed workspace ls`; keeping
// `repo ls` to the repos is the same split `workspace ls` makes from the other
// direction.
func runRepoOnlyList(jsonOut bool) error {
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	// Owners are intentionally dropped here; this view is repos only.
	rows, _, err := collectRepoList(c)
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Repos []repoRow `json:"repos"`
		}{rows})
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stdout, nothingTrackedHint)
		return nil
	}
	// This is a single, standalone table — not the multi-section `shed ls`
	// overview — so it drops the section caption (a lone table needs no label)
	// and its rows sit flush-left (indent "") rather than nested two spaces in.
	writeReposSection(os.Stdout, rows, "", false)
	return nil
}

// runOwnerOnlyList backs `shed owner ls`: the tracked owners only — the whole
// users/orgs whose repos sync auto-adds — without the Repos or Workspaces
// sections that top-level `shed ls` adds. The mirror of `shed repo ls` from the
// owner side; `shed ls` shows all three.
func runOwnerOnlyList(jsonOut bool) error {
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	// Repos are intentionally dropped here; this view is owners only.
	_, owners, err := collectRepoList(c)
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Owners []ownerRow `json:"owners"`
		}{owners})
	}
	if len(owners) == 0 {
		fmt.Fprintln(os.Stdout, noOwnersTrackedHint)
		return nil
	}
	// A single, standalone table — it drops the section caption and its rows sit
	// flush-left (indent ""), matching `shed repo ls`, rather than nested as in
	// `shed ls`.
	writeOwnersSection(os.Stdout, owners, "", false)
	return nil
}

// collectRepoList gathers the repo and owner rows behind `ls`, probing each
// repo's checkout presence and its mirror's meta for the last-sync time. The
// probes are deliberately cheap (a stat and a small metadata read, no size
// walk or git subprocess), so listing stays fast even when the repo count is
// large (a tracked owner may pull in dozens). Workspace state is gathered
// separately by collectWorkspaces.
func collectRepoList(c *config.Config) ([]repoRow, []ownerRow, error) {
	rows := make([]repoRow, 0, len(c.Repos))
	for _, r := range c.Repos {
		name, err := r.ResolvedName()
		if err != nil {
			return nil, nil, errs.Wrap(errs.Config, err)
		}
		row := repoRow{Name: name, URL: r.URL, Track: r.Track, Source: r.Source, LastSyncAt: nil}
		if catalog.Exists(name) {
			row.Path = catalog.Path(name)
		}
		if key, err := r.MirrorKey(); err == nil {
			if st := mirror.StatusFor(key, name); st != nil {
				if !st.LastSyncAt.IsZero() {
					row.LastSyncAt = st.LastSyncAt.UTC().Format(time.RFC3339)
				}
				row.LastError = st.LastError
			}
		}
		rows = append(rows, row)
	}
	owners := make([]ownerRow, 0, len(c.Owners))
	for _, o := range c.Owners {
		name, err := o.ResolvedName()
		if err != nil {
			return nil, nil, errs.Wrap(errs.Config, err)
		}
		owners = append(owners, ownerRow{Name: name, URL: o.URL, RepoCount: len(c.ReposForOwner(name))})
	}
	return rows, owners, nil
}

// collectWorkspaces returns the workspaces derived from the library's repos,
// with their dirty/unpushed/age state. Unlike collectRepoList this runs git
// per workspace, but the workspace count is naturally small (one per task in
// flight, reclaimed by `prune`), so the cost stays bounded.
func collectWorkspaces(c *config.Config) ([]workspace.Info, error) {
	names := make([]string, 0, len(c.Repos))
	for _, r := range c.Repos {
		if n, err := r.ResolvedName(); err == nil {
			names = append(names, n)
		}
	}
	infos, err := workspace.List(names)
	if err != nil {
		return nil, errs.Wrap(errs.Config, err)
	}
	return infos, nil
}

// nothingTrackedHint is shown by the human-readable `ls` views when the library
// is empty, pointing a newcomer at the command that fills it.
const nothingTrackedHint = "(nothing tracked yet — add a repo with `shed add <url>`)"

// noOwnersTrackedHint is shown by `shed owner ls` when no owners are tracked.
// It is owner-specific: nothingTrackedHint speaks to an empty *library* (no
// repos at all), but `shed owner ls` can have repos and still no owners.
const noOwnersTrackedHint = "(no owners tracked yet — track one with `shed owner add <owner>`)"

// writeLibrary renders the human-readable `ls` overview to out: a captioned
// section per kind of thing shed manages, so a newcomer can tell at a glance
// what each table is. Sections with no rows are omitted, except that
// workspaceHint keeps the Workspaces section (with a "how to create one" hint)
// when there are repos but no workspaces yet — the common first-run state.
func writeLibrary(out io.Writer, owners []ownerRow, repos []repoRow, workspaces []workspace.Info, workspaceHint bool) error {
	if len(owners) == 0 && len(repos) == 0 && len(workspaces) == 0 {
		fmt.Fprintln(out, nothingTrackedHint)
		return nil
	}
	first := true
	gap := func() {
		if !first {
			fmt.Fprintln(out)
		}
		first = false
	}
	if len(owners) > 0 {
		gap()
		// The overview captions and indents each section's rows under its
		// heading so the sections read as a nested hierarchy.
		writeOwnersSection(out, owners, "  ", true)
	}
	if len(repos) > 0 {
		gap()
		// The overview captions and indents each section's rows under its
		// heading so the sections read as a nested hierarchy.
		writeReposSection(out, repos, "  ", true)
	}
	if len(workspaces) > 0 || (workspaceHint && len(repos) > 0) {
		gap()
		writeWorkspacesSection(out, workspaces)
	}
	return nil
}

// writeOwnersSection renders the Tracked Owners table. caption controls whether
// the "Tracked Owners" heading precedes it: the multi-section `shed ls` overview
// passes true so the heading distinguishes this table from the sibling Repos and
// Workspaces sections, while the standalone `shed owner ls` passes false — a lone
// table needs no label. indent is prepended to each table row (not the caption):
// the overview passes two spaces so the rows nest under their heading, while the
// standalone view passes "" so its single table sits flush-left — the same split
// writeReposSection makes.
func writeOwnersSection(out io.Writer, owners []ownerRow, indent string, caption bool) {
	if caption {
		fmt.Fprintln(out, "Tracked Owners")
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, indent+"OWNER\tREPOS")
	for _, o := range owners {
		fmt.Fprintf(w, "%s%s\t%d\n", indent, o.Name, o.RepoCount)
	}
	w.Flush()
}

// writeReposSection renders the Repos table. caption controls whether the
// "Repos" heading precedes it: the multi-section `shed ls` overview passes true
// so the heading distinguishes this table from the sibling Tracked Owners and
// Workspaces sections, while the standalone `shed repo ls` passes false — a lone
// table needs no label. indent is prepended to each table row (not the caption):
// the overview passes two spaces so the rows nest visually under their heading,
// while the standalone view passes "" so its single table sits flush-left.
func writeReposSection(out io.Writer, repos []repoRow, indent string, caption bool) {
	if caption {
		fmt.Fprintln(out, "Repos")
	}
	// The "FROM" column only matters when an owner auto-added some repo;
	// hide it otherwise so the common (no-owners) case isn't cluttered with
	// a column of em-dashes.
	showSource := false
	for _, r := range repos {
		if r.Source != "" {
			showSource = true
			break
		}
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if showSource {
		fmt.Fprintln(w, indent+"NAME\tTRACK\tSYNCED\tFROM")
	} else {
		fmt.Fprintln(w, indent+"NAME\tTRACK\tSYNCED")
	}
	for _, r := range repos {
		if showSource {
			source := "—"
			if r.Source != "" {
				source = r.Source
			}
			fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\n", indent, r.Name, trackLabel(r), lastSyncLabel(r), source)
		} else {
			fmt.Fprintf(w, "%s%s\t%s\t%s\n", indent, r.Name, trackLabel(r), lastSyncLabel(r))
		}
	}
	w.Flush()
}

// trackLabel renders a repo's TRACK cell: the pinned branch or tag, or
// "(default)" for the upstream default branch.
func trackLabel(r repoRow) string {
	if r.Track == "" {
		return "(default)"
	}
	return r.Track
}

// lastSyncLabel renders a repo's last-sync cell: a relative time (or "never"),
// annotated when the most recent fetch failed so its stored copy is known stale.
func lastSyncLabel(r repoRow) string {
	last := "never"
	if ts, ok := r.LastSyncAt.(string); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			last = relTime(t)
		}
	}
	if r.LastError != "" {
		last += "  ⚠ sync failing"
	}
	return last
}

// sortWorkspacesByAge returns infos ordered most-recently-active first, so the
// workspace a user just touched sits at the top of the `ls` Workspaces table.
// It ranks by the same ACTIVE column the table shows (reflog-based lastActivity),
// breaking ties by repo name then branch so the order is deterministic. The
// input is left untouched — a copy is sorted, since callers share the slice.
func sortWorkspacesByAge(infos []workspace.Info) []workspace.Info {
	sorted := make([]workspace.Info, len(infos))
	copy(sorted, infos)
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].Age.Equal(sorted[b].Age) {
			if sorted[a].Name != sorted[b].Name {
				return sorted[a].Name < sorted[b].Name
			}
			return sorted[a].Branch < sorted[b].Branch
		}
		return sorted[a].Age.After(sorted[b].Age)
	})
	return sorted
}

func writeWorkspacesSection(out io.Writer, infos []workspace.Info) {
	fmt.Fprintln(out, "Workspaces")
	if len(infos) == 0 {
		fmt.Fprintln(out, "  (none yet — create one with `shed workspace new <repo> <branch>`)")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tREPO\tDIRTY\tUNPUSHED\tACTIVE")
	for _, i := range sortWorkspacesByAge(infos) {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
			i.Branch, i.Name, dirtyLabel(i.Dirty), unpushedLabel(i.Unpushed), relTime(i.Age))
	}
	w.Flush()
}

// recentWorkspaceReposLimit caps how many repos the session-context snapshot
// lists, so a heavily-used shed surfaces only the handful the user is actively
// working in rather than its whole history of workspaces.
const recentWorkspaceReposLimit = 10

// repoActivity pairs a repo with the age of its most recently active workspace
// (the workspace ACTIVE column — reflog-based lastActivity).
type repoActivity struct {
	Name string
	Age  time.Time
}

// recentWorkspaceRepos collapses workspaces to one entry per repo, keeping each
// repo's newest workspace activity, and returns them most-recent first, capped
// at limit (limit <= 0 means no cap). Ranking reuses the same workspace ACTIVE column the
// `ls` Workspaces section shows, so "recent" means the same thing in both places.
func recentWorkspaceRepos(infos []workspace.Info, limit int) []repoActivity {
	newest := make(map[string]time.Time, len(infos))
	for _, i := range infos {
		if cur, ok := newest[i.Name]; !ok || i.Age.After(cur) {
			newest[i.Name] = i.Age
		}
	}
	repos := make([]repoActivity, 0, len(newest))
	for name, age := range newest {
		repos = append(repos, repoActivity{Name: name, Age: age})
	}
	// Most recent first; the repo name breaks ties so the order is deterministic
	// (the map iteration above is randomized) and callers can rely on it.
	sort.Slice(repos, func(a, b int) bool {
		if repos[a].Age.Equal(repos[b].Age) {
			return repos[a].Name < repos[b].Name
		}
		return repos[a].Age.After(repos[b].Age)
	})
	if limit > 0 && len(repos) > limit {
		repos = repos[:limit]
	}
	return repos
}

// writeRecentWorkspaceRepos renders the recent-workspace repos as a small table
// (REPO, and how long ago that repo's newest workspace was last active).
func writeRecentWorkspaceRepos(out io.Writer, repos []repoActivity) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  REPO\tLAST WORKSPACE")
	for _, r := range repos {
		fmt.Fprintf(w, "  %s\t%s\n", r.Name, relTime(r.Age))
	}
	w.Flush()
}

// recentWorkspaceReposText renders, for embedding in session context, the repos
// the user has most recently had a workspace in: one row per repo, ranked by its
// newest workspace's age and capped at recentWorkspaceReposLimit. It replaces
// dumping the whole `ls` library, which a tracked owner can balloon to dozens of
// repos — the agent can still run `shed ls` for the full picture. Best-effort:
// returns "" if the library can't be read or no workspace exists yet, so a
// config hiccup never breaks session startup and a workspace-less shed simply
// omits the section.
func recentWorkspaceReposText() string {
	c, err := config.Load()
	if err != nil {
		return ""
	}
	workspaces, err := collectWorkspaces(c)
	if err != nil || len(workspaces) == 0 {
		return ""
	}
	var buf bytes.Buffer
	writeRecentWorkspaceRepos(&buf, recentWorkspaceRepos(workspaces, recentWorkspaceReposLimit))
	return buf.String()
}
