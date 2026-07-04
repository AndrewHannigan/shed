// Package workspace handles creation, inspection, and removal of
// writable workspaces derived from catalog repos.
//
// A workspace is a completely ordinary git repo: a plain local clone of a
// catalog repo (objects hardlink from the shared mirror, so creation is fast
// and never touches the network) whose origin is immediately re-pointed at
// the real upstream. Normal branching, committing, and pushing all work, its
// removal is a plain delete, and git's own auto-gc keeps a long-lived
// workspace healthy — shed never maintains workspaces.
package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewHannigan/shed/pkg/catalog"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

const mirrorLockTimeout = 2 * time.Second

// lfsSkip keeps the clone from invoking the LFS smudge filter: the mirror
// never smudges, so it has pointer files only and a smudging clone would
// fail (or hit the network). LFS blobs are pulled explicitly afterwards —
// see New.
var lfsSkip = []string{"GIT_LFS_SKIP_SMUDGE=1"}

// Info is a single workspace's state for listing.
type Info struct {
	Name     string    `json:"name"` // repo name e.g. "github.com/foo/bar"
	Branch   string    `json:"branch"`
	Path     string    `json:"path"`
	Dirty    bool      `json:"dirty"`
	Unpushed int       `json:"unpushed"` // -1 if no upstream set
	Age      time.Time `json:"age_mtime"`
}

// PathFor returns the absolute workspace path. Does not check existence.
func PathFor(name, branch string) string {
	return paths.WorkspacePath(name, branch)
}

// Exists returns true if the workspace dir contains a .git directory.
func Exists(name, branch string) bool {
	p := PathFor(name, branch)
	s, err := os.Stat(filepath.Join(p, ".git"))
	return err == nil && s.IsDir()
}

// Source describes the catalog repo a new workspace forks from, resolved by
// the caller from config + the synced mirror.
type Source struct {
	Repo      string            // catalog repo name, e.g. "github.com/foo/bar@v2-7-stable"
	MirrorKey string            // mirror identity, e.g. "github.com/foo/bar"
	Track     string            // short name of the ref the catalog checks out
	URL       string            // real upstream URL — becomes the workspace's origin
	Git       map[string]string // per-repo git config seeded at clone time
}

// New creates a new workspace: a plain `git clone` of the catalog repo
// (purely local — objects hardlink from the mirror's store through the
// worktree) followed by `git remote set-url origin <upstream>`, so the result
// is indistinguishable from an ordinary clone of the real upstream. Returns
// the absolute workspace path and any non-fatal warnings (e.g. LFS blobs
// unavailable offline).
//
// If name exists as an upstream branch, it is checked out. Otherwise a new
// branch called name is created off base — defaulting to the catalog's own
// track, so a workspace made from airflow@v2-7-stable bases on v2-7-stable.
//
// A plain clone sees the mirror's local branches (one per branch-tracked
// catalog) and its tags, so those bases are a single `clone --branch`. Any
// other upstream branch (e.g. reviewing a colleague's feature branch) is a
// two-step: clone the catalog, then fetch that one ref directly from the
// mirror — still offline, still cheap; the fetched delta is copied rather
// than hardlinked, which is fine at workspace lifetimes.
//
// gitConfig from Source.Git is seeded into the new workspace's .git/config at
// clone time via `git clone --config`, so the repo's configured git options
// apply to every later git command in the workspace — including the user's
// own. Keys are validated by config before reaching here.
func New(src Source, name, base string) (path string, warnings []string, err error) {
	// Guard the path-forming inputs so a name/branch can't escape
	// WorkspacesDir (filepath.Join would resolve a ".." away). base only ever
	// becomes a git ref, but validating it too keeps option-injection out of
	// `git clone --branch`.
	if err := paths.ValidateName(src.Repo); err != nil {
		return "", nil, err
	}
	if err := paths.ValidateBranch(name); err != nil {
		return "", nil, err
	}
	if base != "" {
		if err := paths.ValidateBranch(base); err != nil {
			return "", nil, err
		}
	}
	if !catalog.Valid(src.Repo) {
		return "", nil, fmt.Errorf("repo not present; run `shed sync %s` first", src.Repo)
	}
	wsPath := PathFor(src.Repo, name)
	if err := clearHalfCreated(wsPath); err != nil {
		return "", nil, err
	}

	// Shared lock: many workspace creations may clone concurrently, but never
	// while sync or prune holds the mirror exclusively (a clone racing a
	// repack is the one hazard). Two separate acquisitions when a caller also
	// synced — never an in-place flock upgrade (deadlocks).
	lock, err := mirror.AcquireLock(src.MirrorKey, false, mirrorLockTimeout)
	if err != nil {
		return "", nil, err
	}
	defer lock.Unlock()

	plan, err := planCheckout(src, name, base)
	if err != nil {
		return "", nil, err
	}

	if err := os.MkdirAll(filepath.Dir(wsPath), 0755); err != nil {
		return "", nil, err
	}
	// Claim the destination atomically: two concurrent creations of the same
	// name both pass the stat-based checks above, and the loser's clone
	// failing on "already exists" must never be answered by deleting the
	// winner's live workspace. Mkdir is the arbiter — exactly one process
	// owns the path from here on, and every cleanup below removes only a
	// directory this process created. git clones happily into an existing
	// empty directory.
	if err := os.Mkdir(wsPath, 0755); err != nil {
		if os.IsExist(err) {
			return "", nil, fmt.Errorf("workspace already exists at %s (created concurrently?)", wsPath)
		}
		return "", nil, err
	}
	if err := cloneWithRetry(src, plan, wsPath); err != nil {
		return "", nil, err
	}
	if plan.fetchRef != "" {
		// The two-step path: the ref exists only as refs/remotes/origin/* in
		// the mirror, which a plain clone does not copy; fetch just that ref
		// from the mirror (offline) and check it out.
		mirrorPath := paths.MirrorPath(src.MirrorKey)
		refspec := "+refs/remotes/origin/" + plan.fetchRef + ":refs/remotes/origin/" + plan.fetchRef
		if err := gitx.RunEnv(wsPath, lfsSkip, "fetch", "--", mirrorPath, refspec); err != nil {
			os.RemoveAll(wsPath)
			return "", nil, err
		}
	}
	for _, args := range plan.postClone {
		if err := gitx.RunEnv(wsPath, lfsSkip, args...); err != nil {
			os.RemoveAll(wsPath)
			return "", nil, err
		}
	}
	// From here on the workspace is a normal clone of the real upstream.
	if err := gitx.Run(wsPath, "remote", "set-url", "origin", src.URL); err != nil {
		os.RemoveAll(wsPath)
		return "", nil, err
	}
	// The clone above skipped LFS smudging (the mirror holds pointer files
	// only); now that origin points at the real upstream, resolve the blobs.
	// Failure is a warning, not an error: an offline LFS workspace gets
	// pointer files and says so.
	if usesLFS(wsPath) {
		if _, lfsErr := exec.LookPath("git-lfs"); lfsErr != nil {
			warnings = append(warnings, "repo uses git LFS but git-lfs is not installed; files are pointer stubs")
		} else if err := gitx.Run(wsPath, "lfs", "pull"); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not fetch LFS objects (offline?); files are pointer stubs: %v", err))
		}
	}
	return wsPath, warnings, nil
}

// checkoutPlan is how New realizes the requested branch from what the mirror
// has on hand.
type checkoutPlan struct {
	cloneBranch string     // --branch value; "" clones the catalog's HEAD
	fetchRef    string     // non-catalog branch to fetch from the mirror after clone
	postClone   [][]string // git commands run in the workspace after clone/fetch
}

// planCheckout decides the clone/fetch/checkout steps for a requested
// workspace name and base, against the refs available in the mirror.
func planCheckout(src Source, name, base string) (checkoutPlan, error) {
	mirrorPath := paths.MirrorPath(src.MirrorKey)

	// A workspace named after an existing upstream branch checks that branch
	// out (with origin/<name> as its upstream, like a clone --branch would).
	if ok, err := gitx.RefExists(mirrorPath, "refs/remotes/origin/"+name); err != nil {
		return checkoutPlan{}, err
	} else if ok {
		if local, _ := gitx.RefExists(mirrorPath, "refs/heads/"+name); local {
			// The branch is a catalog branch, so the clone sees it directly.
			return checkoutPlan{cloneBranch: name}, nil
		}
		return checkoutPlan{
			fetchRef:  name,
			postClone: [][]string{{"checkout", "-b", name, "origin/" + name}},
		}, nil
	}

	// Otherwise: a new branch called name, forked from base (default: the
	// catalog's own track).
	target := base
	if target == "" {
		target = src.Track
	}
	newBranch := []string{"checkout", "-b", name}
	if local, _ := gitx.RefExists(mirrorPath, "refs/heads/"+target); local {
		return checkoutPlan{cloneBranch: target, postClone: [][]string{newBranch}}, nil
	}
	// Tags are copied by a plain local clone, so `--branch <tag>` works and
	// leaves HEAD detached; the new branch is then created from it.
	if tag, _ := gitx.RefExists(mirrorPath, "refs/tags/"+target); tag {
		return checkoutPlan{cloneBranch: target, postClone: [][]string{newBranch}}, nil
	}
	if remote, _ := gitx.RefExists(mirrorPath, "refs/remotes/origin/"+target); remote {
		return checkoutPlan{
			fetchRef: target,
			postClone: [][]string{
				{"checkout", "--no-track", "-b", name, "refs/remotes/origin/" + target},
			},
		}, nil
	}
	return checkoutPlan{}, fmt.Errorf("base %q not found upstream (no such branch or tag)", target)
}

// cloneWithRetry clones the catalog into dest (an empty directory this
// process already claimed via Mkdir), retrying once after a cleanup —
// insurance against losing a race with a rogue (agent-run) gc repacking the
// mirror mid-clone; shed's own gc never runs while creation holds the shared
// lock. The re-claim between attempts keeps the Mkdir arbitration honest: if
// another process takes the path the moment we release it, we back off with
// the original error rather than touch their directory.
func cloneWithRetry(src Source, plan checkoutPlan, dest string) error {
	err := runClone(src, plan, dest)
	if err == nil {
		return nil
	}
	os.RemoveAll(dest)
	if mkErr := os.Mkdir(dest, 0755); mkErr != nil {
		return err
	}
	if err2 := runClone(src, plan, dest); err2 == nil {
		return nil
	}
	os.RemoveAll(dest)
	return err
}

func runClone(src Source, plan checkoutPlan, dest string) error {
	cmd := exec.Command("git", cloneArgs(paths.CatalogPath(src.Repo), plan.cloneBranch, dest, src.Git)...)
	cmd.Env = append(os.Environ(), lfsSkip...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cloneArgs builds the `git clone` argv. Each --config <key>=<value> persists
// into the new clone's .git/config; they are emitted in sorted order for
// deterministic behavior. Keys are validated by config (no leading "-") so
// they can't be parsed as git options. The trailing "--" terminates options
// so a path beginning with "-" can't be parsed as a git flag (argument
// injection); source and dest are strictly positional.
func cloneArgs(catalogPath, branch, dest string, gitConfig map[string]string) []string {
	args := []string{"clone"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	for _, k := range sortedKeys(gitConfig) {
		args = append(args, "--config", k+"="+gitConfig[k])
	}
	return append(args, "--", catalogPath, dest)
}

// clearHalfCreated inspects an already-existing workspace path. A directory
// whose origin still points into shed's own data dir is the crash window
// between clone and `remote set-url` — the mirror's pre-receive hook keeps
// any push there failing loudly, and here it is repaired by replacement.
// Anything else that exists is a real workspace and an error.
func clearHalfCreated(wsPath string) error {
	if _, err := os.Stat(wsPath); err != nil {
		return nil // nothing there
	}
	if originIsShedOwned(wsPath) {
		return os.RemoveAll(wsPath)
	}
	return fmt.Errorf("workspace already exists at %s", wsPath)
}

// HalfCreated reports whether the directory at (repo, name) is a half-created
// workspace leftover — one whose origin still points into shed's own data dir
// because a crash hit between clone and `remote set-url`. Callers use it to
// let creation repair-by-replacement instead of refusing with "already
// exists".
func HalfCreated(repo, name string) bool {
	p := PathFor(repo, name)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	return originIsShedOwned(p)
}

func originIsShedOwned(wsPath string) bool {
	origin, err := gitx.Output(wsPath, "remote", "get-url", "origin")
	return err == nil && strings.HasPrefix(origin, paths.DataDir()+string(filepath.Separator))
}

// usesLFS reports whether any committed .gitattributes declares the LFS
// filter. Exit status 1 from git grep means "no match", any other failure is
// treated the same — the caller only skips an optional lfs pull.
func usesLFS(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "grep", "-l", "--cached", "-e", "filter=lfs",
		"--", ".gitattributes", ":(glob)**/.gitattributes")
	return cmd.Run() == nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// List returns all workspaces present on disk, scoped to the given repo
// names (so the repo/branch split is unambiguous).
func List(repoNames []string) ([]Info, error) {
	var out []Info
	for _, name := range repoNames {
		repoDir := filepath.Join(paths.WorkspacesDir(), filepath.FromSlash(name))
		if _, err := os.Stat(repoDir); err != nil {
			continue
		}
		walkErr := filepath.Walk(repoDir, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() {
				return nil
			}
			// A dir containing .git is a workspace root. Don't recurse further.
			if s, err := os.Stat(filepath.Join(p, ".git")); err == nil && s.IsDir() {
				rel, err := filepath.Rel(repoDir, p)
				if err != nil || rel == "." {
					return nil
				}
				out = append(out, infoFor(name, filepath.ToSlash(rel), p))
				return filepath.SkipDir
			}
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	return out, nil
}

func infoFor(name, branch, path string) Info {
	i := Info{Name: name, Branch: branch, Path: path, Unpushed: -1}
	i.Dirty, _ = isDirty(path)
	if n, ok := unpushedCount(path); ok {
		i.Unpushed = n
	}
	i.Age = lastActivity(path)
	return i
}

func isDirty(path string) (bool, error) {
	cmd := exec.Command("git", "-C", path, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// unpushedCount returns (count, true) if the branch has an upstream;
// (0, false) if no upstream is configured.
func unpushedCount(path string) (int, bool) {
	cmd := exec.Command("git", "-C", path, "rev-list", "--count", "@{u}..HEAD")
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, false
	}
	return n, true
}

// lastActivity reports when the workspace was last touched, used for its ACTIVE
// column and for prune's age threshold. It reads the timestamp of the newest
// reflog entry — i.e. when the most recent action happened *in this workspace*
// (the clone, a commit, a checkout) — not the date of the commit that entry
// points at.
//
// The distinction matters: a workspace cloned from a repo whose newest commit
// is years old should report its own creation age, not the commit's. The
// reflog's oldest entry is always the clone, so the newest entry's time is
// never older than creation — it reads as the creation time for an untouched
// workspace and advances to the commit time once work lands, which is what the
// ACTIVE column should show.
func lastActivity(path string) time.Time {
	// %gd with --date=unix renders the reflog selector as "HEAD@{<unix>}",
	// the entry's own time. (Plain %ct/%cd would give the pointed-at commit's
	// date instead, the very thing we're avoiding.)
	cmd := exec.Command("git", "-C", path, "log", "-g", "-1", "--format=%gd", "--date=unix")
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}
	}
	return parseReflogUnix(string(out))
}

// parseReflogUnix extracts the unix seconds from a "<ref>@{<unix>}" reflog
// selector (as emitted by `git log -g --format=%gd --date=unix`) and returns
// the corresponding time, or the zero time if it can't be parsed.
func parseReflogUnix(selector string) time.Time {
	open := strings.IndexByte(selector, '{')
	end := strings.IndexByte(selector, '}')
	if open < 0 || end <= open+1 {
		return time.Time{}
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(selector[open+1:end]), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// LandedInDefault reports whether the workspace's branch has already landed in
// its remote default branch — that is, the branch tip (HEAD) is an ancestor of
// refs/remotes/origin/HEAD, so every commit is already contained in the default
// branch. This catches merge- and rebase-merged work even when no PR is
// associated (e.g. a direct push or a local merge).
//
// hasOwnCommits distinguishes a branch whose own commits landed (a real merge)
// from one that never committed anything: a freshly created workspace whose tip
// is still a commit on the default branch's first-parent mainline has not
// "merged" anything, it simply never diverged. It is only meaningful when
// landed is true. prune treats it as load-bearing: a workspace is reclaimed for
// having landed only when its own commits made it in. An empty workspace whose
// tip merely sits on the default branch (landed, !hasOwnCommits) has nothing to
// reclaim and is kept, so a fresh workspace is never deleted just for not having
// diverged yet.
//
// defaultBranch is the default branch's short name (e.g. "main") for use in
// messages. landed is false when the default branch can't be resolved (treated
// conservatively as "not landed", so a stale or missing origin/HEAD never
// causes a deletion) or when the branch is itself the default branch (so a
// checkout of main is never pruned just for containing itself).
//
// Comparing against the last-fetched origin/HEAD means staleness only ever
// makes this more conservative: an out-of-date default branch yields a false
// negative (keep), never a false positive (delete).
func LandedInDefault(path, branch string) (landed, hasOwnCommits bool, defaultBranch string, err error) {
	def, err := defaultBranchShortName(path)
	if err != nil {
		// Can't resolve the default branch — stay conservative and keep.
		return false, false, "", nil
	}
	if def == branch {
		return false, false, def, nil
	}
	cmd := exec.Command("git", "-C", path,
		"merge-base", "--is-ancestor", "HEAD", "refs/remotes/origin/HEAD")
	err = cmd.Run()
	if err == nil {
		// Contained in the default branch. A branch that merged work in sits
		// off the default branch's first-parent mainline (its tip is a merge
		// commit's second parent); a branch that never committed has a tip that
		// is itself a mainline commit. If we can't tell, assume it had commits
		// so we never wrongly claim "no commits" — the worst case is the old,
		// broader "merged" wording.
		onMainline, mlErr := tipOnDefaultMainline(path)
		if mlErr != nil {
			return true, true, def, nil
		}
		return true, !onMainline, def, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, false, def, nil
	}
	return false, false, def, err
}

// tipOnDefaultMainline reports whether the workspace's HEAD is itself one of the
// default branch's commits — reachable by following refs/remotes/origin/HEAD's
// first parents only. Used to tell a real merge apart from a branch that never
// committed once HEAD is known to be contained in the default branch.
func tipOnDefaultMainline(path string) (bool, error) {
	headOut, err := exec.Command("git", "-C", path, "rev-parse", "HEAD").Output()
	if err != nil {
		return false, err
	}
	head := strings.TrimSpace(string(headOut))
	out, err := exec.Command("git", "-C", path,
		"rev-list", "--first-parent", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == head {
			return true, nil
		}
	}
	return false, nil
}

// defaultBranchShortName resolves the workspace's remote default branch to its
// short name (e.g. "main") via refs/remotes/origin/HEAD.
func defaultBranchShortName(path string) (string, error) {
	cmd := exec.Command("git", "-C", path,
		"symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	short := strings.TrimSpace(string(out))
	return strings.TrimPrefix(short, "origin/"), nil
}

// CheckClean returns (dirty, unpushed, error). If the workspace is
// clean, returns (false, 0, nil).
func CheckClean(path string) (bool, int, error) {
	dirty, err := isDirty(path)
	if err != nil {
		return false, 0, err
	}
	unpushed, ok := unpushedCount(path)
	if !ok {
		unpushed = 0 // no upstream → no unpushed commits to report
	}
	return dirty, unpushed, nil
}

// Remove deletes the workspace dir.
func Remove(name, branch string) error {
	p := PathFor(name, branch)
	if !Exists(name, branch) {
		return fmt.Errorf("workspace not found at %s", p)
	}
	return os.RemoveAll(p)
}

// RepoDir returns the directory holding all workspaces for a repo.
func RepoDir(name string) string {
	return filepath.Join(paths.WorkspacesDir(), filepath.FromSlash(name))
}

// RemoveAllForRepo deletes every workspace belonging to the repo, along
// with the now-empty per-repo workspace directory. Returns nil if no
// workspaces exist. Workspaces are plain writable clones, so a single
// os.RemoveAll suffices.
func RemoveAllForRepo(name string) error {
	dir := RepoDir(name)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	paths.PruneEmptyDirs(filepath.Dir(dir), paths.WorkspacesDir())
	return nil
}

// ListAll scans the workspaces directory directly and returns every
// workspace found, without consulting config. Use this when config may
// be missing or about to be deleted (e.g. purge). The Name field holds
// the repo-relative path on disk (repo name plus branch); callers that
// only need paths and dirty/unpushed status should prefer this over List.
func ListAll() ([]Info, error) {
	root := paths.WorkspacesDir()
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	walkErr := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		if s, err := os.Stat(filepath.Join(p, ".git")); err == nil && s.IsDir() {
			rel, err := filepath.Rel(root, p)
			if err != nil || rel == "." {
				return nil
			}
			out = append(out, infoFor(filepath.ToSlash(rel), "", p))
			return filepath.SkipDir
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}
