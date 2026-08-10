// Package paths centralizes every on-disk location shed touches.
// All functions return absolute paths.
package paths

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const appName = "shed"

// ConfigDir returns ~/.config/shed (honoring XDG_CONFIG_HOME).
func ConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, appName)
	}
	return filepath.Join(home(), ".config", appName)
}

// DataDir returns ~/.shed.
func DataDir() string {
	return filepath.Join(home(), "."+appName)
}

func ConfigFile() string     { return filepath.Join(ConfigDir(), "config.toml") }
func ConfigLockFile() string { return filepath.Join(ConfigDir(), ".lock") }

// Initialized reports whether `shed init` has been run, by checking for the two
// roots init creates that every command depends on: the config file (under
// ConfigDir) and the data directory. It is a deliberately cheap presence check
// — two stats, no parsing — so every command can run it on startup to fail with
// a clear "run shed init" message instead of behaving like an empty catalog. It
// does not validate contents; a malformed config still surfaces its own error
// when loaded.
func Initialized() bool {
	if _, err := os.Stat(ConfigFile()); err != nil {
		return false
	}
	fi, err := os.Stat(DataDir())
	return err == nil && fi.IsDir()
}

func ReposDir() string      { return filepath.Join(DataDir(), "repos") }
func WorkspacesDir() string { return filepath.Join(DataDir(), "workspaces") }
func LogsDir() string       { return filepath.Join(DataDir(), "logs") }

// InternalDir is shed's plumbing bucket. One rule decides what lives here: if
// shed prints a path for the user or an agent to visit, it is top-level under
// DataDir; everything else — mirrors, lock files, error records, history —
// stays under .internal so the user-facing layout is exactly two concepts
// (repos and workspaces) plus logs.
func InternalDir() string { return filepath.Join(DataDir(), ".internal") }

// MirrorsDir holds the fetch-only mirror repos: one per unique upstream,
// shared by every catalog repo tracking that upstream. Mirrors are plumbing —
// never printed as a destination.
func MirrorsDir() string { return filepath.Join(InternalDir(), "mirrors") }

// MirrorPath returns the on-disk path for a mirror, keyed by the URL-derived
// "host/owner/repo" identity (see DefaultName) — never by the raw URL string,
// so two transports of one upstream share a mirror.
func MirrorPath(key string) string {
	return filepath.Join(MirrorsDir(), filepath.FromSlash(key))
}

// MirrorLockFile and MirrorMetaFile are the mirror's sidecars, kept inside its
// .git so they live and die with the mirror.
func MirrorLockFile(key string) string {
	return filepath.Join(MirrorPath(key), ".git", "shed.lock")
}

// MirrorCreateLockFile is the creation lock for a mirror: a sibling of the
// mirror directory (it cannot live inside — the mirror doesn't exist yet)
// that serializes concurrent creations of the same upstream.
func MirrorCreateLockFile(key string) string {
	return MirrorPath(key) + ".create.lock"
}

func MirrorMetaFile(key string) string {
	return filepath.Join(MirrorPath(key), ".git", "shed.meta")
}

func BgSyncLockFile() string { return filepath.Join(InternalDir(), "bg-sync.lock") }
func BgSyncLogFile() string  { return filepath.Join(LogsDir(), "bg-sync.log") }

// SyncErrorDir holds standalone failure records for repos that failed their
// very first sync — before a mirror (and thus its .git/shed.meta sidecar)
// ever existed. Kept outside ReposDir so a record can never be mistaken for a
// materialized catalog repo.
func SyncErrorDir() string { return filepath.Join(InternalDir(), "sync-errors") }

// SyncErrorFile is the JSON failure record for a repo whose first sync failed,
// keyed by repo name (e.g. "github.com/foo/bar" → "sync-errors/github.com/foo/bar.json").
func SyncErrorFile(name string) string {
	return filepath.Join(SyncErrorDir(), filepath.FromSlash(name)+".json")
}

// HistoryFile is the JSON-Lines log of recent shed commands (one event
// per line). HistoryTrimMarkerFile holds the RFC3339 timestamp of the last
// trim check, used to debounce truncation of the history file.
func HistoryFile() string           { return filepath.Join(InternalDir(), "history.jsonl") }
func HistoryTrimMarkerFile() string { return filepath.Join(InternalDir(), "history-trim") }

// CatalogPath returns the on-disk path for a named catalog repo
// (e.g. "github.com/foo/bar" or "github.com/foo/bar@v2" →
// "<DataDir>/repos/github.com/foo/bar[@v2]").
func CatalogPath(name string) string {
	return filepath.Join(ReposDir(), filepath.FromSlash(name))
}

// WorkspacePath returns the on-disk path for a (repo, branch) workspace.
// Branch with slashes becomes nested dirs.
func WorkspacePath(name, branch string) string {
	return filepath.Join(WorkspacesDir(), filepath.FromSlash(name), filepath.FromSlash(branch))
}

// WorkspaceSessionFile returns the session-link sidecar path inside a
// workspace's .git dir. It records which agent session created the workspace
// so `shed resume` can reopen it. Living inside the workspace means it is
// removed automatically when the workspace is — the link never outlives or
// endangers its workspace.
func WorkspaceSessionFile(name, branch string) string {
	return filepath.Join(WorkspacePath(name, branch), ".git", "shed.session")
}

// SessionsPendingDir holds short-lived session→workspace intents recorded by
// the pre-exec hook before `workspace new` runs. `workspace new` finalizes the
// matching intent into a WorkspaceSessionFile and removes it. Kept under
// InternalDir so it is local state, never confused with a repo or workspace.
func SessionsPendingDir() string { return filepath.Join(InternalDir(), "sessions-pending") }

// SessionPendingFile is the pending-intent record keyed by the (globally
// unique) workspace name. The name is the unambiguous join key between the
// hook (which parsed it from the command) and `workspace new` (which has it as
// an argument).
func SessionPendingFile(workspaceName string) string {
	return filepath.Join(SessionsPendingDir(), filepath.FromSlash(workspaceName)+".json")
}

// checkSafeRelPath verifies p is a relative, slash-separated path that cannot
// escape a base directory once joined into one: no absolute prefix, no Windows
// volume/backslash, and no ".." or empty segment. Repo names and branches are
// always "/"-separated regardless of host OS, so we split on "/" rather than
// the OS separator. Without this, a name or branch like "../../etc/x" would
// make filepath.Join resolve outside ReposDir/WorkspacesDir.
func checkSafeRelPath(p string) error {
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return errors.New("must be a relative path")
	}
	if strings.ContainsRune(p, '\\') {
		return errors.New("must not contain backslashes")
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "..":
			return errors.New(`must not contain a ".." segment`)
		case "":
			return errors.New("must not contain empty path segments")
		}
	}
	return nil
}

// ValidateName reports an error when name is not a safe relative repo name —
// one that, joined under ReposDir or WorkspacesDir, could escape it. Called
// when names enter config (user `--name` overrides, URL-derived defaults, and
// owner-discovered repos) so a traversing name is rejected before it ever
// reaches a path.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("repo name is empty")
	}
	if err := checkSafeRelPath(name); err != nil {
		return fmt.Errorf("repo name %q is unsafe: %w", name, err)
	}
	return nil
}

// ValidateBranch is ValidateName's analog for branch names, which become
// nested directories under a workspace and must likewise stay contained. It
// additionally rejects a leading "-" so the branch can't be parsed as an
// option when passed to git (e.g. `git clone --branch`, `git checkout -b`);
// git refs cannot begin with "-" anyway.
func ValidateBranch(branch string) error {
	if branch == "" {
		return errors.New("branch is empty")
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("branch %q is unsafe: must not start with %q", branch, "-")
	}
	if err := checkSafeRelPath(branch); err != nil {
		return fmt.Errorf("branch %q is unsafe: %w", branch, err)
	}
	return nil
}

// TrackKind classifies a config `track` value: a full-ref form pins the kind,
// a bare short name resolves branch-first (matching `git clone --branch`).
type TrackKind int

const (
	TrackAny    TrackKind = iota // bare short name: prefer a branch, fall back to a tag
	TrackBranch                  // "heads/<name>" form
	TrackTag                     // "tags/<name>" form
)

// ParseTrack splits a config `track` value into its short name and kind. The
// full-ref forms ("heads/2.7.3", "tags/2.7.3") are the escape hatch for
// branch/tag name collisions and pin the kind; anything else is a short name
// resolved branch-first. Full-ref forms exist for resolution only — naming and
// workspace creation use the short name.
func ParseTrack(track string) (short string, kind TrackKind) {
	if rest, ok := strings.CutPrefix(track, "heads/"); ok {
		return rest, TrackBranch
	}
	if rest, ok := strings.CutPrefix(track, "tags/"); ok {
		return rest, TrackTag
	}
	return track, TrackAny
}

// ValidateTrack guards a config `track` value before it reaches any git
// command or path derivation: the short portion gets the same checks as a
// branch (no leading "-", safe relative path), applied after stripping an
// optional heads/ or tags/ prefix.
func ValidateTrack(track string) error {
	short, _ := ParseTrack(track)
	if short == "" {
		return errors.New("track is empty")
	}
	if strings.HasPrefix(short, "-") {
		return fmt.Errorf("track %q is unsafe: must not start with %q", track, "-")
	}
	if err := checkSafeRelPath(short); err != nil {
		return fmt.Errorf("track %q is unsafe: %w", track, err)
	}
	return nil
}

// SanitizeTrack maps a track's short name to the form that appears in a
// derived repo name: slashes become "-" so the "@<track>" suffix stays one
// leaf directory ("release/2.8" → "release-2.8"). The mapping is lossy by
// design — config remains the source of truth — and config.Validate rejects
// two tracks that sanitize identically.
func SanitizeTrack(track string) string {
	short, _ := ParseTrack(track)
	return strings.ReplaceAll(short, "/", "-")
}

// DefaultRepoName returns the default name for a (url, track) pair:
// "host/path" for the default branch, "host/path@<sanitized-track>" for a
// tracked ref.
func DefaultRepoName(rawURL, track string) (string, error) {
	base, err := DefaultName(rawURL)
	if err != nil {
		return "", err
	}
	if track == "" {
		return base, nil
	}
	return base + "@" + SanitizeTrack(track), nil
}

// WriteFileAtomic writes data to path atomically: it writes a sibling temp
// file (same directory, so the rename stays on one filesystem) and renames it
// over path, so a reader or a crash never sees a half-written file. When path
// already exists its permission bits are preserved; otherwise defaultPerm is
// used. The temp file is chmod'd to the chosen mode before the rename, so the
// result does not depend on the process umask, and the temp is cleaned up if a
// later step fails.
func WriteFileAtomic(path string, data []byte, defaultPerm os.FileMode) error {
	perm := defaultPerm
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// NormalizeURL expands a user-supplied repo reference into a full git URL.
// Full URLs (anything with a "://" scheme) and scp-style remotes
// (git@host:path) are returned unchanged. A bare reference with no scheme is
// treated as shorthand and expanded so the common cases just work:
//
//	octocat                         -> https://github.com/octocat
//	octocat/Hello-World             -> https://github.com/octocat/Hello-World
//	github.com/octocat              -> https://github.com/octocat
//	gitlab.com/foo/bar              -> https://gitlab.com/foo/bar
//
// A leading segment that looks like a host (contains "." or ":") is taken as
// the host and only given an https:// scheme; otherwise the reference is
// GitHub shorthand (owner or owner/repo) and is resolved against github.com.
func NormalizeURL(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return s
	}
	// Already a full URL (https://, ssh://, git://) or scp-style git@host:path.
	if strings.Contains(s, "://") || isSCPLike(s) {
		return s
	}
	s = strings.Trim(s, "/")
	// A leading segment that looks like a host (a dot, or a host:port colon)
	// means the user gave host/owner[/repo] without a scheme; just add https://.
	if first, _, _ := strings.Cut(s, "/"); strings.ContainsAny(first, ".:") {
		return "https://" + s
	}
	// Otherwise it's GitHub shorthand: owner or owner/repo.
	return "https://github.com/" + s
}

// isSCPLike reports whether s is a scp-style remote (user@host:path with no
// scheme), the one no-scheme form NormalizeURL must leave untouched. Mirrors
// the detection in ParseURL.
func isSCPLike(s string) bool {
	if at := strings.Index(s, "@"); at > 0 {
		return strings.Contains(s[at+1:], ":")
	}
	return false
}

// IsSSHURL reports whether a git URL uses SSH transport — either an explicit
// ssh:// scheme or the scp-style git@host:path form. HTTPS, HTTP, and git://
// URLs are not SSH.
func IsSSHURL(rawURL string) bool {
	return strings.HasPrefix(rawURL, "ssh://") || isSCPLike(rawURL)
}

// AlternateProtocolURL returns the same repo addressed over the other
// transport: an HTTPS URL becomes scp-style SSH (git@host:owner/repo.git), and
// an SSH/scp URL becomes HTTPS (https://host/owner/repo). It returns "" when
// rawURL cannot be parsed or uses a scheme with no obvious counterpart (e.g.
// git://), so callers can simply skip the swap. Used by `add` to recover when
// the chosen protocol can't authenticate but the other one can.
func AlternateProtocolURL(rawURL string) string {
	host, path, err := ParseURL(rawURL)
	if err != nil {
		return ""
	}
	switch {
	case IsSSHURL(rawURL):
		return "https://" + host + "/" + path
	case strings.HasPrefix(rawURL, "https://"), strings.HasPrefix(rawURL, "http://"):
		return "git@" + host + ":" + path + ".git"
	default:
		return ""
	}
}

// ParseURL extracts (host, path) from a git URL. Handles both standard
// URLs (https://, ssh://, git://) and scp-style (git@github.com:foo/bar.git).
// Path has any trailing ".git" stripped.
func ParseURL(rawURL string) (host, path string, err error) {
	// scp-style: user@host:path (no scheme, host is before ":")
	if !strings.Contains(rawURL, "://") {
		if at := strings.Index(rawURL, "@"); at > 0 {
			rest := rawURL[at+1:]
			if colon := strings.Index(rest, ":"); colon > 0 {
				host = rest[:colon]
				path = strings.TrimPrefix(rest[colon+1:], "/")
				path = strings.TrimSuffix(path, ".git")
				if host == "" || path == "" {
					return "", "", fmt.Errorf("could not parse host or path from %q", rawURL)
				}
				return host, path, nil
			}
		}
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parse URL %q: %w", rawURL, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("missing host in URL %q", rawURL)
	}
	host = u.Host
	path = strings.TrimPrefix(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return "", "", fmt.Errorf("missing path in URL %q", rawURL)
	}
	return host, path, nil
}

// DefaultName returns "host/path" as the default repo name for a URL.
func DefaultName(rawURL string) (string, error) {
	host, path, err := ParseURL(rawURL)
	if err != nil {
		return "", err
	}
	return host + "/" + path, nil
}

// IsOwnerURL reports whether a git URL points at a bare owner (user or org)
// rather than a specific repo — i.e. its path is a single segment
// ("github.com/octocat") with no "<owner>/<repo>" tail. Returns an
// error only if the URL itself cannot be parsed.
func IsOwnerURL(rawURL string) (bool, error) {
	_, path, err := ParseURL(rawURL)
	if err != nil {
		return false, err
	}
	return !strings.Contains(path, "/"), nil
}

// DefaultOwnerName returns "host/owner" as the default name for an owner URL.
// It errors if the URL's path has more than one segment (i.e. it looks like a
// repo URL, not an owner URL).
func DefaultOwnerName(rawURL string) (string, error) {
	host, path, err := ParseURL(rawURL)
	if err != nil {
		return "", err
	}
	if strings.Contains(path, "/") {
		return "", fmt.Errorf("URL %q looks like a repo, not an owner (path %q has multiple segments)", rawURL, path)
	}
	return host + "/" + path, nil
}

// PruneEmptyDirs removes dir and each now-empty ancestor, walking up
// until (but not including) stop. It stops at the first directory that
// is non-empty or cannot be removed. Used to clean up the intermediate
// host/owner dirs left behind when a repo's leaf dir is deleted.
func PruneEmptyDirs(dir, stop string) {
	stopPrefix := stop + string(filepath.Separator)
	for dir != stop && strings.HasPrefix(dir, stopPrefix) {
		if err := os.Remove(dir); err != nil {
			return // non-empty or gone; nothing more to prune
		}
		dir = filepath.Dir(dir)
	}
}

// Display returns a path with $HOME collapsed to "~" for human-readable output.
func Display(p string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return p
	}
	if p == h {
		return "~"
	}
	if strings.HasPrefix(p, h+string(os.PathSeparator)) {
		return "~" + p[len(h):]
	}
	return p
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		// UserHomeDir can fail in pathological envs; fall back to $HOME
		// or "/" as a last resort. Don't panic — callers will fail later
		// with a clearer error when paths don't resolve.
		if h := os.Getenv("HOME"); h != "" {
			return h
		}
		return "/"
	}
	return h
}
