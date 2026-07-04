package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [<repo>]",
		Short: "Report sync health; with a repo, show its error and the likely fix",
		Long: `Report which tracked repos failed their most recent sync.

With no argument, lists every repo whose last sync attempt failed (their
stored copies are stale). With a repo, prints the full captured error, when
it happened, and a suggested fix.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return errs.Wrap(errs.Config, err)
			}
			if len(args) == 1 {
				return runStatusRepo(c, args[0])
			}
			return runStatusSummary(c)
		},
	}
}

// syncFailure describes a repo whose most recent sync attempt failed.
type syncFailure struct {
	Name        string
	LastSyncAt  time.Time // last *successful* sync; zero if never
	LastErrorAt time.Time
	LastError   string
}

// collectSyncFailures returns every tracked repo whose meta records a failed
// most-recent attempt (LastError set), newest failure first. Shared by the
// `status` summary and the session-context staleness banner.
func collectSyncFailures(c *config.Config) []syncFailure {
	var out []syncFailure
	for _, r := range c.Repos {
		name, err := r.ResolvedName()
		if err != nil {
			continue
		}
		m := loadRepoFailure(&r, name)
		if m == nil {
			continue
		}
		out = append(out, syncFailure{
			Name:        name,
			LastSyncAt:  m.LastSyncAt,
			LastErrorAt: m.LastErrorAt,
			LastError:   m.LastError,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastErrorAt.After(out[j].LastErrorAt) })
	return out
}

// loadRepoFailure returns the active failure for a repo — from its mirror's
// meta (which merges the shared fetch state with the repo's own catalog
// record) when the mirror exists, or from the standalone first-sync store
// when a failed first clone left no mirror — or nil when the last attempt was
// clean. The two stores are mutually exclusive in practice: mirror meta
// exists only once the mirror does, and a successful sync clears the
// standalone record.
func loadRepoFailure(r *config.Repo, name string) *mirror.CatalogStatus {
	if key, err := r.MirrorKey(); err == nil {
		if m := mirror.StatusFor(key, name); m != nil && m.LastError != "" {
			return m
		}
	}
	if m, _ := mirror.LoadFirstSyncError(name); m != nil && m.LastError != "" {
		return m
	}
	return nil
}

func runStatusSummary(c *config.Config) error {
	fails := collectSyncFailures(c)
	if len(fails) == 0 {
		fmt.Printf("All %d tracked repos synced cleanly.\n", len(c.Repos))
		return nil
	}
	fmt.Printf("%d of %d repos have a failing sync:\n", len(fails), len(c.Repos))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, f := range fails {
		fmt.Fprintf(w, "  %s\t%s\n", f.Name, lastGoodSync(f.LastSyncAt))
	}
	w.Flush()
	fmt.Println("Run `shed status <repo>` for the full error and suggested fix.")
	return nil
}

func runStatusRepo(c *config.Config, arg string) error {
	r, err := c.Resolve(arg)
	if err != nil {
		return err // already coded errs.NotFound (exit 2), like every other command
	}
	name, err := r.ResolvedName()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	var meta *mirror.CatalogStatus
	if key, err := r.MirrorKey(); err == nil {
		meta = mirror.StatusFor(key, name)
	}
	// A failed first clone leaves no mirror (hence no meta); its error
	// lives in the standalone store. Surface that as the active failure.
	if meta == nil {
		if fe, _ := mirror.LoadFirstSyncError(name); fe != nil && fe.LastError != "" {
			meta = fe
		}
	}

	fmt.Println(name)
	if meta == nil {
		fmt.Printf("  not synced yet — run `shed sync %s`\n", name)
		return nil
	}
	fmt.Printf("  last good sync:  %s\n", stampLine(meta.LastSyncAt))
	if meta.LastError == "" {
		fmt.Println("  status:          ok")
		return nil
	}
	if !meta.LastErrorAt.IsZero() {
		fmt.Printf("  last attempt:    failed %s ago (%s)\n",
			relDuration(time.Since(meta.LastErrorAt)), meta.LastErrorAt.Format(stampFmt))
	}
	fmt.Println("  error:")
	for _, line := range wrapIndent(meta.LastError, "    ", 76) {
		fmt.Println(line)
	}
	fmt.Printf("  likely cause: %s\n", likelyCause(meta.LastError, name, r.URL))
	return nil
}

const stampFmt = "2006-01-02 15:04 MST"

// lastGoodSync renders the age of the last successful sync for summary rows.
func lastGoodSync(t time.Time) string {
	if t.IsZero() {
		return "never synced successfully"
	}
	return "last good sync " + relTime(t)
}

// stampLine renders an absolute+relative timestamp for the detail view.
func stampLine(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%s   (%s)", relTime(t), t.Format(stampFmt))
}

// likelyCause maps a captured sync error to a one-line suggested fix. Pure
// string heuristics over git's output — best-effort, never authoritative.
// url is the repo's configured URL, used to tailor the auth remedy to the
// transport (SSH key vs. HTTPS credential).
func likelyCause(errText, name, url string) string {
	low := strings.ToLower(errText)
	sync := fmt.Sprintf("`shed sync %s`", name)
	switch {
	case isAuthError(low):
		return authFixHint(url) + ", then " + sync
	case looksGoneUpstream(low):
		return fmt.Sprintf("repo may be renamed or deleted upstream — verify it exists, or remove it with `shed rm %s`", name)
	case containsAny(low, "could not resolve host", "connection refused", "connection timed out", "timed out", "network is unreachable", "temporary failure in name resolution"):
		return "network — likely transient; check your connection and re-run " + sync
	case strings.Contains(low, "locked"):
		return "another sync held the lock — re-run " + sync + " shortly"
	case containsAny(low, "no space left", "disk full"):
		return "disk full — free space, then re-run " + sync
	default:
		return "re-run " + sync + "; see the full output above"
	}
}

// looksGoneUpstream reports whether git's (already-lowercased) output indicates
// the remote repository no longer resolves — deleted, renamed, or (for a
// private repo) access revoked, which GitHub reports identically as "Repository
// not found". Shared by status reporting and sync's gone-upstream handling.
// It is a heuristic over git's text — an expired credential yields the same
// "not found" — so it only ever downgrades severity or informs, never deletes.
func looksGoneUpstream(low string) bool {
	return containsAny(low, "repository not found", "not found", "does not exist", "404")
}

// isAuthError reports whether git's (already-lowercased) output looks like an
// authentication or authorization failure, over either HTTPS or SSH. Shared
// by status reporting and the `add` preflight so both classify identically.
func isAuthError(low string) bool {
	return containsAny(low,
		"could not read username", "could not read password",
		"authentication failed", "permission denied",
		"403 forbidden", "terminal prompts disabled",
		"invalid credentials", "host key verification failed",
	)
}

// authFixHint gives a protocol-aware remedy for an auth failure: SSH-key
// guidance for git@/ssh:// remotes, credential/token guidance for HTTPS. The
// gh suggestion only makes sense for HTTPS (it configures git's credential
// helper), so steering SSH users to it — as the old single message did —
// would have sent them down a dead end.
func authFixHint(url string) string {
	if paths.IsSSHURL(url) {
		return "SSH auth — ensure your key is loaded (`ssh-add -l`) and authorized for the host"
	}
	return "HTTPS auth — run `gh auth login` or configure a git credential helper/token"
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// wrapIndent splits s on existing newlines, then word-wraps each segment to
// width columns, prefixing every output line with indent.
func wrapIndent(s, indent string, width int) []string {
	var out []string
	for _, seg := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		words := strings.Fields(seg)
		if len(words) == 0 {
			continue
		}
		line := words[0]
		for _, wd := range words[1:] {
			if len(line)+1+len(wd) > width {
				out = append(out, indent+line)
				line = wd
				continue
			}
			line += " " + wd
		}
		out = append(out, indent+line)
	}
	return out
}
