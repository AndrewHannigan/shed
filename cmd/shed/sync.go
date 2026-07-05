package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/catalog"
	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/forge"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

const syncLockTimeout = 5 * time.Minute

// syncDefaultJobs is the default concurrency for `sync`, also used when
// `add` triggers an implicit sync of the just-added entry.
const syncDefaultJobs = 4

func newSyncCmd() *cobra.Command {
	var (
		jobs        int
		ifOlderThan time.Duration
		jsonOut     bool
	)
	cmd := &cobra.Command{
		Use:   "sync [<name>...]",
		Short: "Fetch each upstream's mirror and refresh the read-only repos",
		Long: `sync refreshes every tracked repo (or the named subset) in two phases:
first each upstream's shared mirror is fetched — one network fetch per
upstream, no matter how many versions of it you track — then each repo's
checkout is fast-forwarded to its tracked branch (a tracked tag never
moves) and re-locked read-only.

With --if-older-than, skip mirrors fetched within the given duration.
Runs in parallel up to --jobs (one job per mirror).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(args, jobs, ifOlderThan, jsonOut)
		},
	}
	cmd.Flags().IntVarP(&jobs, "jobs", "j", syncDefaultJobs, "max concurrent mirror fetches")
	cmd.Flags().DurationVar(&ifOlderThan, "if-older-than", 0, "skip mirrors fetched within this duration (e.g. 1h)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit NDJSON results")
	return cmd
}

type syncResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // "ok" | "skipped" | "error" | "gone"
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	Note       string `json:"note,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"` // the shared mirror's size

	// locked marks an error caused by a mirror-lock timeout (vs a network
	// failure), so callers can classify it without matching on Error text.
	// Not serialized — the message in Error carries the user-facing detail.
	locked bool
}

// syncTarget is one catalog repo in a sync's scope.
type syncTarget struct {
	name, url, track string
	git              map[string]string
}

// mirrorJob is one upstream's worth of sync work: a single network fetch of
// the shared mirror, then a local update per catalog repo tracking it.
type mirrorJob struct {
	key   string // mirror identity (host/owner/repo)
	url   string // fetch transport: the first config entry's URL
	repos []syncTarget
	// keyErr is set when no safe mirror identity could be derived from the
	// URL. Such a job must fail its repos without touching the disk: a raw
	// URL is not a validated relative path, and joining one under MirrorsDir
	// could escape it.
	keyErr error
}

func runSync(names []string, jobs int, ifOlderThan time.Duration, jsonOut bool) error {
	if err := gitx.RequireGit(); err != nil {
		return errs.Wrap(errs.MissingDep, err)
	}
	if jobs < 1 {
		jobs = 1
	}

	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	if len(c.Repos) == 0 && len(c.Owners) == 0 {
		if !jsonOut {
			fmt.Fprintln(os.Stderr, "no repos in config; add with `shed add <url>`")
		}
		return nil
	}

	// Discover repos for any owners in scope and add new ones to config, so
	// repos that appeared upstream since the last sync are picked up and
	// fetched in this same pass. Failures here are warned about and skipped —
	// already-known repos still sync (graceful degradation when gh is absent).
	if owners := ownersInScope(c, names); len(owners) > 0 {
		reconcileOwners(owners, forge.ListOwnerRepos, jsonOut)
		c, err = config.Load() // reload to include newly added repos
		if err != nil {
			return errs.Wrap(errs.Config, err)
		}
	}

	// Advisory config findings (e.g. two entries sharing a mirror but
	// disagreeing on transport) — warnings, never failures.
	for _, w := range c.Warnings() {
		warnSync("%s", w)
	}

	targets, err := resolveSyncTargets(c, names)
	if err != nil {
		return err
	}
	mirrorJobs := groupByMirror(targets)

	if !jsonOut {
		fmt.Printf("syncing %s across %s (jobs=%d)\n",
			pluralize(len(targets), "repo"), pluralize(len(mirrorJobs), "mirror"), jobs)
	}

	// Stream git's live clone/fetch progress meter only when a single mirror is
	// in scope and stderr is a terminal — the `shed add <repo>` case, and a
	// targeted `shed sync <repo>`. With several mirrors fetching in parallel
	// their meters would interleave into noise, and piped/JSON output wants no
	// cursor control codes, so both fall back to the quiet per-line summary.
	var progress io.Writer
	if !jsonOut && len(mirrorJobs) == 1 && isTerminal(os.Stderr) {
		progress = os.Stderr
	}

	// Branches every mirror must keep, derived from the FULL config (not just
	// this sync's scope): a scoped sync must never treat a sibling catalog's
	// branch as stray.
	keep := expectedBranchesByMirror(c)

	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var enc *json.Encoder
	if jsonOut {
		enc = json.NewEncoder(os.Stdout)
	}
	results := make([]syncResult, 0, len(targets))

	for _, job := range mirrorJobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(job mirrorJob) {
			defer wg.Done()
			defer func() { <-sem }()
			rs := syncMirrorJob(job, keep[job.key], ifOlderThan, progress)
			mu.Lock()
			for _, r := range rs {
				results = append(results, r)
				if jsonOut {
					_ = enc.Encode(r)
				} else {
					printSyncLine(r)
				}
			}
			mu.Unlock()
		}(job)
	}
	wg.Wait()

	reconcileGone(results, jsonOut)
	if len(names) == 0 {
		reportOrphanCatalogs(c, jsonOut)
	}
	return summarizeSync(results, len(targets), jsonOut)
}

// syncMirrorJob runs one upstream's sync: the network phase (fetch the shared
// mirror, refresh its notion of the default branch, stamp its meta) under the
// mirror's exclusive lock, then — with the lock released between phases — a
// local, deterministic update of each catalog repo. keep lists local branches
// that must survive the stray-branch sweep even if this job doesn't cover
// their repos.
func syncMirrorJob(job mirrorJob, keep map[string]bool, ifOlderThan time.Duration, progress io.Writer) []syncResult {
	start := time.Now()

	// No safe mirror identity: fail every repo of the job without touching
	// the disk. The standalone first-sync records keep the failure visible in
	// `shed status` (they are keyed by validated repo names, so they're safe
	// to write even when the URL isn't).
	if job.keyErr != nil {
		out := make([]syncResult, 0, len(job.repos))
		for _, t := range job.repos {
			r := syncResult{Name: t.name, Status: "error", Error: job.keyErr.Error()}
			_ = mirror.RecordFirstSyncError(t.name, job.keyErr.Error())
			r.DurationMs = time.Since(start).Milliseconds()
			out = append(out, r)
		}
		return out
	}

	if !mirror.Exists(job.key) {
		if err := mirror.Create(job.url, job.key, progress); err != nil {
			return failAllFetch(job, start, err)
		}
	}

	lock, err := mirror.AcquireLock(job.key, true, syncLockTimeout)
	if err != nil {
		return failAllLock(job, start, err)
	}

	fetch := true
	if ifOlderThan > 0 {
		if m, _ := mirror.LoadMeta(job.key); m != nil && !m.LastSyncAt.IsZero() &&
			time.Since(m.LastSyncAt) < ifOlderThan {
			fetch = false
		}
	}
	// A failed fetch (offline, auth, upstream down) is recorded and reported
	// as a failure — the repo IS stale — but it does not abort the local
	// phase: the mirror still holds everything from the last successful
	// fetch, so checkouts are created or kept from that state. This is what
	// lets a newly added version of an already-mirrored repo materialize
	// offline, the same graceful degradation `workspace new` promises.
	var fetchErr error
	if fetch {
		if err := mirror.Fetch(job.key, progress); err != nil {
			fetchErr = err
		} else if err := mirror.RefreshHead(job.key); err != nil {
			fetchErr = err
		}
		if fetchErr != nil {
			_ = mirror.RecordFetchError(job.key, fetchErr.Error())
		} else {
			_ = mirror.RecordFetchOK(job.key, time.Now().UTC())
		}
	}

	// Resolve each repo's track against the fetched refs — the pre-check that
	// turns a deleted upstream branch into "track 'x' no longer exists
	// upstream" instead of a git internals error — and use the resolved set
	// (plus keep) to sweep stray local branches out of the mirror. The sweep
	// is cleanup, not repair; skip it when the refs are known stale.
	refs := make(map[string]catalog.Ref, len(job.repos))
	resolveErrs := make(map[string]error)
	expected := make(map[string]bool, len(keep))
	for b := range keep {
		expected[b] = true
	}
	for _, t := range job.repos {
		ref, err := catalog.ResolveTrack(job.key, t.track)
		if err != nil {
			resolveErrs[t.name] = err
			continue
		}
		refs[t.name] = ref
		if !ref.IsTag {
			expected[ref.Short] = true
		}
	}
	if fetchErr == nil {
		mirror.PruneStrayBranches(job.key, expected)
	}
	lock.Unlock() // released between the network phase and the catalog phase

	// The mirror's size is shared by every repo of the job (catalog checkouts
	// carry no objects of their own), so walk it once, not once per repo.
	size, _ := mirror.Size(job.key)

	results := make([]syncResult, 0, len(job.repos))
	for _, t := range job.repos {
		results = append(results, syncCatalog(job.key, t, refs[t.name], resolveErrs[t.name], fetchErr, !fetch, size, start))
	}
	return results
}

// syncCatalog runs the local phase for one repo: create/repair/fast-forward
// its catalog checkout under the mirror's exclusive lock, and record the
// outcome on the mirror's meta. With fetchErr set the phase still runs — the
// checkout is materialized or kept from the last-synced state — but the repo
// reports the fetch failure and no fresh sync is recorded (the data really is
// stale).
func syncCatalog(key string, t syncTarget, ref catalog.Ref, resolveErr, fetchErr error, mirrorSkipped bool, size int64, start time.Time) syncResult {
	r := syncResult{Name: t.name}

	lock, err := mirror.AcquireLock(key, true, syncLockTimeout)
	if err != nil {
		// Deliberately NOT persisted to the mirror meta: we don't hold the
		// lock, so a mutateMeta here would race the very process that does
		// (and could clobber its fresh records). The holder is actively
		// syncing this mirror anyway — its outcome is the truthful record.
		r = fillLockErr(r, key, err)
		r.DurationMs = time.Since(start).Milliseconds()
		return r
	}
	defer lock.Unlock()

	if resolveErr != nil {
		// With a failed fetch, the resolve ran against stale refs — a
		// missing track or apparent emptiness may just mean "never fetched" —
		// so the fetch failure is the truthful report.
		if fetchErr != nil {
			return failFetch(r, start, fetchErr)
		}
		// An upstream with no commits is a state, not an error: nothing to
		// check out yet; the repo materializes on the first sync after
		// upstream gains commits.
		if errors.Is(resolveErr, catalog.ErrEmptyUpstream) {
			_ = mirror.RecordCatalogOK(key, t.name, time.Now().UTC())
			mirror.ClearFirstSyncError(t.name)
			r.Status = "ok"
			r.Note = "empty"
			r.DurationMs = time.Since(start).Milliseconds()
			return r
		}
		return finishErr(r, key, start, resolveErr)
	}

	note, err := catalog.Ensure(key, t.name, ref, t.git)
	if err != nil {
		return finishErr(r, key, start, err)
	}

	if fetchErr != nil {
		// A tag is a permanent pointer: with the tag already in the mirror,
		// the checkout is exact — a fetch could not have changed it — so a
		// failed refresh is harmless for this repo and it counts as ok.
		// (A gone upstream still reports, though: that's a standing reminder
		// to `shed rm`, not a staleness question.)
		if ref.IsTag && !looksGoneUpstream(strings.ToLower(fetchErr.Error())) {
			_ = mirror.RecordCatalogOK(key, t.name, time.Now().UTC())
			mirror.ClearFirstSyncError(t.name)
			r.Status = "ok"
			if note == "created" {
				r.Note = "created from mirror (offline — a tag never changes)"
			} else {
				r.Note = note
			}
			r.SizeBytes = size
			r.DurationMs = time.Since(start).Milliseconds()
			return r
		}
		// A branch checkout really can be behind: the checkout is usable
		// (created or kept from the last-synced state), but the sync itself
		// failed — report that, and leave the meta's sync records untouched
		// so "last sync" keeps meaning "last time the network was reached".
		r = failFetch(r, start, fetchErr)
		if note == "created" {
			r.Note = "checkout created from last-synced state"
		} else {
			r.Note = "checkout kept from last-synced state"
		}
		return r
	}

	if mirrorSkipped && note == "" {
		// No fetch and the checkout was already current: report the skip with
		// the mirror's real last-fetch age, and leave the meta untouched so
		// "last sync" keeps meaning "last actual sync".
		r.Status = "skipped"
		if m, _ := mirror.LoadMeta(key); m != nil && !m.LastSyncAt.IsZero() {
			r.Note = fmt.Sprintf("synced %s ago", relDuration(time.Since(m.LastSyncAt)))
		}
		r.DurationMs = time.Since(start).Milliseconds()
		return r
	}

	_ = mirror.RecordCatalogOK(key, t.name, time.Now().UTC())
	// Success: drop any standalone first-sync failure record from an earlier
	// failed clone so it doesn't keep showing up as stale.
	mirror.ClearFirstSyncError(t.name)

	r.Status = "ok"
	r.Note = note
	r.SizeBytes = size
	r.DurationMs = time.Since(start).Milliseconds()
	return r
}

// syncSingle refreshes one repo — its mirror plus its own catalog — for
// `workspace new`, where exactly one repo is in scope (`add` goes through
// runSync with the new entry's name instead). The stray-branch keep-set is
// still derived from the FULL config: a single-repo refresh must never treat
// a sibling catalog's branch as stray.
func syncSingle(t syncTarget, progress io.Writer) syncResult {
	job := groupByMirror([]syncTarget{t})[0]
	var keep map[string]bool
	if c, err := config.Load(); err == nil {
		keep = expectedBranchesByMirror(c)[job.key]
	}
	return syncMirrorJob(job, keep, 0, progress)[0]
}

// groupByMirror buckets sync targets by their upstream's mirror identity, so
// syncing N versions of one repo costs one network fetch. Order of first
// appearance is preserved; the first entry's URL is the fetch transport.
//
// A URL that yields no identity — unparseable, or parsing to something that
// is not a safe relative path (repo names get this validation at config
// time; mirror keys must get it too, or a hostile URL could place the mirror
// outside MirrorsDir) — produces a job with keyErr set, which fails its
// repos without ever touching the disk.
func groupByMirror(targets []syncTarget) []mirrorJob {
	var jobs []mirrorJob
	index := make(map[string]int)
	for _, t := range targets {
		key, err := paths.DefaultName(t.url)
		if err == nil {
			if verr := paths.ValidateName(key); verr != nil {
				err = fmt.Errorf("unsafe mirror identity derived from URL %q: %w", t.url, verr)
			}
		} else {
			err = fmt.Errorf("cannot derive a mirror identity from URL %q: %w", t.url, err)
		}
		bucket := key
		if err != nil {
			bucket = "\x00invalid\x00" + t.url
		}
		if i, ok := index[bucket]; ok {
			jobs[i].repos = append(jobs[i].repos, t)
			continue
		}
		index[bucket] = len(jobs)
		jobs = append(jobs, mirrorJob{key: key, url: t.url, repos: []syncTarget{t}, keyErr: err})
	}
	return jobs
}

// expectedBranchesByMirror returns, per mirror key, the local branch names the
// FULL config claims — every branch-or-ambiguous track's short name. Used to
// protect sibling catalogs' branches during a scoped sync's stray-branch
// sweep. Default-branch repos resolve their name only at sync time, but their
// branches are checked out by live worktrees and so can't be swept anyway.
func expectedBranchesByMirror(c *config.Config) map[string]map[string]bool {
	out := make(map[string]map[string]bool)
	for _, r := range c.Repos {
		key, err := r.MirrorKey()
		if err != nil || r.Track == "" {
			continue
		}
		short, kind := paths.ParseTrack(r.Track)
		if kind == paths.TrackTag {
			continue
		}
		if out[key] == nil {
			out[key] = make(map[string]bool)
		}
		out[key][short] = true
	}
	return out
}

// failFetch fills one repo's result for a failed network phase: "gone" when
// the remote no longer resolves, "error" otherwise. Nothing is persisted here
// — the mirror-level RecordFetchError already holds the record, which
// StatusFor surfaces for every catalog of the mirror.
func failFetch(r syncResult, start time.Time, err error) syncResult {
	if looksGoneUpstream(strings.ToLower(err.Error())) {
		r.Status = "gone"
	} else {
		r.Status = "error"
	}
	r.Error = err.Error()
	r.DurationMs = time.Since(start).Milliseconds()
	return r
}

// failAllFetch reports a mirror CREATION failure as one result per repo of
// the job — with no mirror on disk there is no last-synced state to fall
// back on, so this is the one network failure that stays a hard stop. Repos
// with no mirror get standalone first-sync records so status has something
// to show. (A failed fetch of an EXISTING mirror does not come here: the
// local phase still runs from the last-synced refs — see syncMirrorJob.)
func failAllFetch(job mirrorJob, start time.Time, err error) []syncResult {
	gone := looksGoneUpstream(strings.ToLower(err.Error()))
	out := make([]syncResult, 0, len(job.repos))
	for _, t := range job.repos {
		r := syncResult{Name: t.name, Error: err.Error()}
		if gone {
			r.Status = "gone"
		} else {
			r.Status = "error"
		}
		if !mirror.Exists(job.key) {
			_ = mirror.RecordFirstSyncError(t.name, err.Error())
		}
		r.DurationMs = time.Since(start).Milliseconds()
		out = append(out, r)
	}
	return out
}

// failAllLock reports a mirror lock acquisition failure for every repo of the
// job. Nothing is persisted to the mirror meta: without the lock a
// read-modify-write of shed.meta would race the holder, and the holder — an
// active sync of this same mirror — is the truthful record anyway.
func failAllLock(job mirrorJob, start time.Time, err error) []syncResult {
	out := make([]syncResult, 0, len(job.repos))
	for _, t := range job.repos {
		r := fillLockErr(syncResult{Name: t.name}, job.key, err)
		r.DurationMs = time.Since(start).Milliseconds()
		out = append(out, r)
	}
	return out
}

// fillLockErr classifies a mirror-lock acquisition failure onto r: a timeout
// becomes the user-facing "locked: ..." message (and sets the locked flag so
// summarizeSync exits with the Locked code), anything else passes through.
func fillLockErr(r syncResult, key string, err error) syncResult {
	r.Status = "error"
	if errors.Is(err, mirror.ErrLocked) {
		r.locked = true
		r.Error = fmt.Sprintf("locked: could not acquire %s within %s (held by another shed process)",
			paths.MirrorLockFile(key), syncLockTimeout)
	} else {
		r.Error = err.Error()
	}
	return r
}

// finishErr records a repo-level sync failure: fills Error/DurationMs and
// persists the failure to the mirror's meta (or the standalone first-sync
// store when no mirror exists) so `ls`, `status`, and the session-context
// banner surface it.
func finishErr(r syncResult, key string, start time.Time, err error) syncResult {
	r.Status = "error"
	r.Error = err.Error()
	r.DurationMs = time.Since(start).Milliseconds()
	if mirror.Exists(key) {
		_ = mirror.RecordCatalogError(key, r.Name, err.Error())
	} else {
		_ = mirror.RecordFirstSyncError(r.Name, err.Error())
	}
	return r
}

// reconcileGone reports the repos whose remote vanished during the fetch pass.
// It only informs; it never deletes — removing a tracked repo (and any
// workspace under it) is always an explicit `shed rm`, never a side effect of
// sync. Each note points the user there. A repo stays "gone" on every sync
// until they remove it, which is the intended standing reminder.
func reconcileGone(results []syncResult, jsonOut bool) {
	// In --json mode the per-repo "gone" record is already on stdout as NDJSON;
	// route these human notes to stderr so they don't corrupt it.
	out := os.Stdout
	if jsonOut {
		out = os.Stderr
	}
	for _, r := range results {
		if r.Status != "gone" {
			continue
		}
		fmt.Fprintf(out, "  note: %s is gone upstream (deleted, renamed, or access revoked)\n", r.Name)
		fmt.Fprintf(out, "        remove it with `shed rm %s`\n", r.Name)
	}
}

// reportOrphanCatalogs notes on-disk repo dirs that no config entry claims —
// the usual cause is a changed `track`, which is an identity change
// (remove-and-add) that leaves the old directory behind. Sync never deletes
// them itself; `shed prune` does.
func reportOrphanCatalogs(c *config.Config, jsonOut bool) {
	onDisk, err := catalog.OnDisk()
	if err != nil || len(onDisk) == 0 {
		return
	}
	known := make(map[string]bool, len(c.Repos))
	for _, r := range c.Repos {
		if n, err := r.ResolvedName(); err == nil {
			known[n] = true
		}
	}
	out := os.Stdout
	if jsonOut {
		out = os.Stderr
	}
	for _, name := range onDisk {
		if known[name] {
			continue
		}
		fmt.Fprintf(out, "  note: %s is on disk but not in the config (changed track?)\n", name)
		fmt.Fprintf(out, "        `shed prune` will remove it\n")
	}
}

func resolveSyncTargets(c *config.Config, names []string) ([]syncTarget, error) {
	if len(names) == 0 {
		out := make([]syncTarget, 0, len(c.Repos))
		for _, r := range c.Repos {
			n, err := r.ResolvedName()
			if err != nil {
				return nil, errs.Wrap(errs.Config, err)
			}
			out = append(out, syncTarget{n, r.URL, r.Track, r.Git})
		}
		return out, nil
	}
	out := make([]syncTarget, 0, len(names))
	seen := make(map[string]bool)
	add := func(t syncTarget) {
		if !seen[t.name] {
			out = append(out, t)
			seen[t.name] = true
		}
	}
	for _, name := range names {
		// A name may be a repo or an owner. Resolve both: a repo expands to
		// itself, an owner expands to its managed repos. Matching both (a rare
		// suffix collision — exact names are unique across the two by config
		// validation) is ambiguous → exit 2 asking for the full name, the same
		// treatment `shed rm` gives it.
		r, repoErr := c.Resolve(name)
		o, ownerErr := c.ResolveOwner(name)
		switch {
		case repoErr == nil && ownerErr == nil:
			rn, _ := r.ResolvedName()
			on, _ := o.ResolvedName()
			return nil, errs.New(errs.NotFound,
				"%q is ambiguous; matches owner %q and repo %q — use the full name", name, on, rn)
		case repoErr == nil:
			n, err := r.ResolvedName()
			if err != nil {
				return nil, errs.Wrap(errs.Config, err)
			}
			add(syncTarget{n, r.URL, r.Track, r.Git})
		case ownerErr == nil:
			on, err := o.ResolvedName()
			if err != nil {
				return nil, errs.Wrap(errs.Config, err)
			}
			for _, rn := range c.ReposForOwner(on) {
				if rr := c.FindByName(rn); rr != nil {
					add(syncTarget{rn, rr.URL, rr.Track, rr.Git})
				}
			}
		default:
			return nil, repoErr // repo not-found/ambiguous message is the common case
		}
	}
	return out, nil
}

// ownerLister lists an owner's repos. forge.ListOwnerRepos in production; a
// fake in tests.
type ownerLister func(ownerURL string, f forge.Filter) ([]forge.Repo, error)

// ownersInScope returns the owners a sync invocation should reconcile: all
// owners when no names are given, otherwise just those names that resolve to
// an owner (repo names are handled separately by resolveSyncTargets).
func ownersInScope(c *config.Config, names []string) []config.Owner {
	if len(names) == 0 {
		return c.Owners
	}
	var owners []config.Owner
	seen := make(map[string]bool)
	for _, name := range names {
		o, err := c.ResolveOwner(name)
		if err != nil {
			continue
		}
		on, _ := o.ResolvedName()
		if !seen[on] {
			owners = append(owners, *o)
			seen[on] = true
		}
	}
	return owners
}

func ownerFilter(o config.Owner) forge.Filter {
	return forge.Filter{
		IncludeForks:    o.IncludeForks,
		IncludeArchived: o.IncludeArchived,
		Visibility:      o.Visibility,
	}
}

// reconcileOwners discovers each owner's repos via list and adds any new ones
// to config as Source-tagged entries. It is additive only — it never removes
// repos that disappeared upstream; a vanished repo is surfaced as "gone" by the
// fetch pass and left for the user to `shed rm`, so sync never deletes a repo
// (or its workspace) on the user's behalf. Discovery failures are warned about
// and skipped so already-known repos still sync (graceful degradation when gh
// is unavailable).
func reconcileOwners(owners []config.Owner, list ownerLister, jsonOut bool) {
	for _, o := range owners {
		ownerName, err := o.ResolvedName()
		if err != nil {
			warnSync("skipping owner with unparseable URL %q: %v", o.URL, err)
			continue
		}
		added, err := reconcileOwner(o, list)
		if err != nil {
			warnSync("could not expand owner %s: %v", ownerName, err)
			continue
		}
		if len(added) > 0 && !jsonOut {
			fmt.Printf("  owner %s: discovered %s\n", ownerName, pluralize(len(added), "new repo"))
		}
	}
}

// reconcileOwner lists one owner's repos (outside the config lock) and appends
// the new ones under the config lock. Returns the resolved names added.
func reconcileOwner(o config.Owner, list ownerLister) (added []string, err error) {
	repos, err := list(o.URL, ownerFilter(o))
	if err != nil {
		return nil, err
	}
	err = config.WithLock(configLockTimeout, func(c *config.Config) error {
		toAdd := newOwnerRepos(c, o, repos)
		if len(toAdd) == 0 {
			return nil
		}
		c.Repos = append(c.Repos, toAdd...)
		for _, r := range toAdd {
			n, _ := r.ResolvedName()
			added = append(added, n)
		}
		return config.Save(c)
	})
	if err != nil {
		if errors.Is(err, config.ErrLocked) {
			return nil, errs.Wrap(errs.Locked, err)
		}
		return nil, err
	}
	return added, nil
}

// newOwnerRepos returns the Repo entries to append for a discovered set,
// skipping any whose resolved name already exists in c (as a user repo, a
// managed repo, or an owner), is in the owner's exclude list, or is a
// duplicate within the discovered batch. Pure, so the additive/dedupe logic
// is unit-testable without gh or disk.
//
// Owner auto-discovery always materializes default-branch repos; `track`
// overrides are added by hand, never auto-generated.
func newOwnerRepos(c *config.Config, o config.Owner, discovered []forge.Repo) []config.Repo {
	ownerName, err := o.ResolvedName()
	if err != nil {
		return nil
	}
	exclude := make(map[string]bool)
	for _, e := range o.Exclude {
		exclude[e] = true
	}
	var toAdd []config.Repo
	queued := make(map[string]bool)
	for _, d := range discovered {
		if d.CloneURL == "" {
			continue
		}
		name, err := paths.DefaultName(d.CloneURL)
		if err != nil {
			continue
		}
		if c.FindByName(name) != nil || c.FindOwnerByName(name) != nil || queued[name] || exclude[name] {
			continue
		}
		queued[name] = true
		toAdd = append(toAdd, config.Repo{URL: d.CloneURL, Source: ownerName})
	}
	return toAdd
}

// warnSync writes a discovery warning to stderr. It always uses stderr so it
// never corrupts NDJSON results on stdout in --json mode.
func warnSync(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "warning: "+format+"\n", a...)
}

func printSyncLine(r syncResult) {
	switch r.Status {
	case "ok":
		if r.Note != "" {
			fmt.Printf("  %s  ✓  %s — %s  (%s)\n", r.Name, humanSize(r.SizeBytes), r.Note, formatMs(r.DurationMs))
		} else {
			fmt.Printf("  %s  ✓  %s  (%s)\n", r.Name, humanSize(r.SizeBytes), formatMs(r.DurationMs))
		}
	case "skipped":
		fmt.Printf("  %s  -  skipped (%s)\n", r.Name, r.Note)
	case "gone":
		if r.Note != "" {
			fmt.Printf("  %s  ⚠  gone upstream — %s\n", r.Name, r.Note)
		} else {
			fmt.Printf("  %s  ⚠  gone upstream\n", r.Name)
		}
	case "error":
		if r.Note != "" {
			fmt.Printf("  %s  ✗  %s — %s\n", r.Name, r.Error, r.Note)
		} else {
			fmt.Printf("  %s  ✗  %s\n", r.Name, r.Error)
		}
	}
}

func summarizeSync(results []syncResult, total int, jsonOut bool) error {
	var ok, skip, gone, errCnt, lockCnt, netCnt int
	for _, r := range results {
		switch r.Status {
		case "ok":
			ok++
		case "skipped":
			skip++
		case "gone":
			gone++
		case "error":
			errCnt++
			if r.locked {
				lockCnt++
			} else {
				netCnt++
			}
		}
	}
	if !jsonOut {
		// "gone upstream" is tallied apart from "failed" so a deleted repo never
		// reads as a sync failure, and only shown when it happened.
		line := fmt.Sprintf("%d of %d ok", ok, total)
		if gone > 0 {
			line += fmt.Sprintf("; %d gone upstream", gone)
		}
		line += fmt.Sprintf("; %d failed; %d skipped", errCnt, skip)
		fmt.Println(line)
	}
	// A vanished remote is an expected lifecycle event, not a failure: it must
	// not flip the exit code. Only lock/network errors do.
	if lockCnt > 0 {
		return errs.New(errs.Locked, "%d repos failed to acquire lock", lockCnt)
	}
	if netCnt > 0 {
		return errs.New(errs.Network, "%d repos failed", netCnt)
	}
	return nil
}
