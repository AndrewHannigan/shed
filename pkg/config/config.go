// Package config defines the on-disk config schema and load/save with
// file-level locking.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/pelletier/go-toml/v2"

	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// Config is the root TOML document.
type Config struct {
	Settings Settings `toml:"settings,omitempty"`
	Repos    []Repo   `toml:"repo,omitempty"`
	Owners   []Owner  `toml:"owner,omitempty"`
}

type Settings struct {
	BgSyncInterval string `toml:"bg_sync_interval,omitempty"`
}

type Repo struct {
	URL  string `toml:"url"`
	Name string `toml:"name,omitempty"`
	// Track pins which upstream ref this repo's checkout follows. Empty means
	// the upstream default branch. A branch advances (fast-forwards) on every
	// sync; a tag never changes. Bare short names resolve branch-first; the
	// full-ref forms "heads/<name>" / "tags/<name>" are the escape hatch when
	// a branch and tag share a name.
	Track string `toml:"track,omitempty"`
	// Source, when set, is the resolved name of the [[owner]] entry that
	// auto-added this repo (see Owner). Empty means the repo was added by
	// the user directly. Auto-managed repos are reconciled on each sync;
	// user-added repos are never touched by owner reconciliation.
	Source string `toml:"source,omitempty"`
	// Git holds git config key/value pairs applied to this repo: reconciled
	// into the catalog checkout's worktree-scoped git config on every sync,
	// and seeded into each new workspace at clone time. shed forwards them
	// verbatim to git and never interprets the keys, so any git config
	// option works without shed code.
	// Set/update only — removing a key here does not unset it from a clone
	// that already has it (re-add the repo to fully reset).
	Git map[string]string `toml:"git,omitempty"`
}

// Owner is a tracked user or org. On each sync, shed lists the owner's
// repos (via `gh`) and materializes any new ones as Source-tagged Repo
// entries, so the rest of shed treats them as ordinary stored repos.
type Owner struct {
	URL             string `toml:"url"`
	Name            string `toml:"name,omitempty"`
	IncludeForks    bool   `toml:"include_forks,omitempty"`
	IncludeArchived bool   `toml:"include_archived,omitempty"`
	// Visibility filters discovered repos: "all" (default), "public", or
	// "private". Empty is treated as "all".
	Visibility string `toml:"visibility,omitempty"`
	// Exclude lists resolved repo names (e.g. "github.com/owner/repo") that
	// should not be auto-added on sync, even though they belong to this owner.
	// Populated automatically when `shed rm` is called on a repo that was
	// added by this owner.
	Exclude []string `toml:"exclude,omitempty"`
}

// ResolvedName returns the effective name for a repo: the explicit Name field
// if set, else the default derived from URL and Track ("host/owner/repo" for
// the default branch, "host/owner/repo@<track>" for a tracked ref — see
// paths.DefaultRepoName).
func (r Repo) ResolvedName() (string, error) {
	if r.Name != "" {
		return r.Name, nil
	}
	return paths.DefaultRepoName(r.URL, r.Track)
}

// MirrorKey returns the identity of the mirror this repo shares: the
// URL-derived "host/owner/repo" path, never the raw URL string, so two config
// entries for one upstream over different transports (https:// and git@…:)
// share one mirror.
func (r Repo) MirrorKey() (string, error) {
	return paths.DefaultName(r.URL)
}

// ResolvedName returns the effective name for an owner: the explicit Name
// field if set, else "host/owner" derived from URL.
func (o Owner) ResolvedName() (string, error) {
	if o.Name != "" {
		return o.Name, nil
	}
	return paths.DefaultOwnerName(o.URL)
}

// ValidateGitConfigKey guards a per-repo git config key before shed forwards
// it to git (via `git config <key> <value>` and `git clone --config
// <key>=<value>`). It is deliberately permissive: it blocks argument
// injection (a key parsed as a flag) and obvious malformations, then leaves
// the rest of git's key grammar to git itself — so shed never has to track
// which keys git accepts.
func ValidateGitConfigKey(key string) error {
	switch {
	case key == "":
		return errors.New("git config key is empty")
	case strings.HasPrefix(key, "-"):
		return fmt.Errorf("git config key %q must not start with '-'", key)
	case strings.ContainsAny(key, " \t\r\n\x00"):
		return fmt.Errorf("git config key %q must not contain whitespace", key)
	case !strings.Contains(key, "."):
		return fmt.Errorf("git config key %q must be of the form section.key", key)
	}
	return nil
}

// ErrLocked is returned when the config lock cannot be acquired in time.
var ErrLocked = errors.New("config locked by another process")

// Load reads the config file. Missing file returns an empty Config (not an
// error). Malformed file returns an error.
func Load() (*Config, error) {
	data, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", paths.ConfigFile(), err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the config atomically (write to .tmp, rename).
func Save(c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.ConfigDir(), 0755); err != nil {
		return err
	}
	data, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	return paths.WriteFileAtomic(paths.ConfigFile(), data, 0644)
}

// WithLock acquires the config lock, runs fn, releases the lock.
// Returns ErrLocked if the lock cannot be acquired within timeout.
func WithLock(timeout time.Duration, fn func(*Config) error) error {
	if err := os.MkdirAll(paths.ConfigDir(), 0755); err != nil {
		return err
	}
	lock := flock.New(paths.ConfigLockFile())
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		// flock reports a timeout as (false, ctx.Err()), so the deadline is
		// what actually signals contention — translate it so callers'
		// errors.Is(err, ErrLocked) classification works.
		if errors.Is(err, context.DeadlineExceeded) {
			return ErrLocked
		}
		return err
	}
	if !locked {
		return ErrLocked
	}
	defer lock.Unlock()
	c, err := Load()
	if err != nil {
		return err
	}
	return fn(c)
}

// Validate enforces invariants: every repo and owner has a URL; every
// (resolved) name is unique across both repos and owners (they share one
// namespace because commands resolve a single argument against both) — which,
// because names embed the sanitized track, also rejects two tracks that
// sanitize to the same on-disk path; every track is safe to hand to git; and
// no two repos share a (mirror, track) pair — two checkouts of one ref would
// be identical read-only trees, and the 1:1 track↔catalog-branch mapping is
// what lets each mirror branch belong to exactly one worktree.
func (c *Config) Validate() error {
	seen := make(map[string]string)      // resolved name -> "repo N" / "owner N"
	seenTrack := make(map[string]string) // mirror key + "\x00" + track -> "repo N"
	for i, r := range c.Repos {
		if r.URL == "" {
			return fmt.Errorf("repo[%d]: url is required", i)
		}
		if r.Track != "" {
			if err := paths.ValidateTrack(r.Track); err != nil {
				return fmt.Errorf("repo[%d] (%q): %w", i, r.URL, err)
			}
		}
		name, err := r.ResolvedName()
		if err != nil {
			return fmt.Errorf("repo[%d] (%q): %w", i, r.URL, err)
		}
		if err := paths.ValidateName(name); err != nil {
			return fmt.Errorf("repo[%d] (%q): %w", i, r.URL, err)
		}
		for key := range r.Git {
			if err := ValidateGitConfigKey(key); err != nil {
				return fmt.Errorf("repo[%d] (%q): %w", i, r.URL, err)
			}
		}
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("name %q appears in both %s and repo %d", name, prev, i)
		}
		seen[name] = fmt.Sprintf("repo %d", i)
		// One repo per (upstream, track), even under explicit name overrides.
		// Keyed by the mirror identity (host/owner/repo), not the raw URL, so
		// the same ref over two transports still counts as a duplicate.
		if key, err := r.MirrorKey(); err == nil {
			tk := key + "\x00" + r.Track
			if prev, ok := seenTrack[tk]; ok {
				return fmt.Errorf("repo %d duplicates %s: same upstream %q and track %q",
					i, prev, key, r.Track)
			}
			seenTrack[tk] = fmt.Sprintf("repo %d", i)
		}
	}
	for i, o := range c.Owners {
		if o.URL == "" {
			return fmt.Errorf("owner[%d]: url is required", i)
		}
		name, err := o.ResolvedName()
		if err != nil {
			return fmt.Errorf("owner[%d] (%q): %w", i, o.URL, err)
		}
		if err := paths.ValidateName(name); err != nil {
			return fmt.Errorf("owner[%d] (%q): %w", i, o.URL, err)
		}
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("name %q appears in both %s and owner %d", name, prev, i)
		}
		seen[name] = fmt.Sprintf("owner %d", i)
	}
	return nil
}

// Warnings returns advisory (non-fatal) config findings. Today that is one
// check: repos that share a mirror (same upstream identity) but disagree on
// transport (https:// vs git@…:). The mirror fetches with the first entry's
// URL, so the other entries' transport choice is silently ignored — worth a
// warning, not an error.
func (c *Config) Warnings() []string {
	firstURL := make(map[string]string) // mirror key -> first URL seen
	var warnings []string
	warned := make(map[string]bool)
	for _, r := range c.Repos {
		key, err := r.MirrorKey()
		if err != nil {
			continue
		}
		prev, ok := firstURL[key]
		if !ok {
			firstURL[key] = r.URL
			continue
		}
		if prev != r.URL && paths.IsSSHURL(prev) != paths.IsSSHURL(r.URL) && !warned[key] {
			warnings = append(warnings, fmt.Sprintf(
				"repos sharing the mirror for %s disagree on transport (%s vs %s); fetches use %s",
				key, prev, r.URL, prev))
			warned[key] = true
		}
	}
	return warnings
}

// FindByName returns the repo entry with the given resolved name, or nil
// if not present.
func (c *Config) FindByName(name string) *Repo {
	for i := range c.Repos {
		if n, err := c.Repos[i].ResolvedName(); err == nil && n == name {
			return &c.Repos[i]
		}
	}
	return nil
}

// FindOwnerByName returns the owner entry with the given resolved name, or
// nil if not present.
func (c *Config) FindOwnerByName(name string) *Owner {
	for i := range c.Owners {
		if n, err := c.Owners[i].ResolvedName(); err == nil && n == name {
			return &c.Owners[i]
		}
	}
	return nil
}

// ReposForOwner returns the resolved names of every repo whose Source equals
// the given owner name (i.e. repos auto-added by that owner).
func (c *Config) ReposForOwner(owner string) []string {
	var names []string
	for i := range c.Repos {
		if c.Repos[i].Source != owner {
			continue
		}
		if n, err := c.Repos[i].ResolvedName(); err == nil {
			names = append(names, n)
		}
	}
	return names
}

// Resolve finds the config entry matching name — the one resolution rule
// used shed-wide: an exact match on the resolved name wins; otherwise an
// unambiguous suffix match on path-segment ("/") boundaries is used. Returns an errs.Coded with
// NotFound when nothing matches or when a suffix matches more than one
// repo (the message lists the candidates so the user can disambiguate).
func (c *Config) Resolve(name string) (*Repo, error) {
	if r := c.FindByName(name); r != nil {
		return r, nil
	}
	var matches []*Repo
	var candidates []string
	for i := range c.Repos {
		n, err := c.Repos[i].ResolvedName()
		if err != nil {
			continue
		}
		if strings.HasSuffix(n, "/"+name) {
			matches = append(matches, &c.Repos[i])
			candidates = append(candidates, n)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, errs.New(errs.NotFound, "repo %q is not in the config", name)
	default:
		return nil, errs.New(errs.NotFound,
			"repo %q is ambiguous; matches: %s", name, strings.Join(candidates, ", "))
	}
}

// ResolveOwner finds the owner entry matching name using the same rule as
// Resolve (exact resolved-name match, else unambiguous suffix match on "/"
// boundaries). Returns an errs.Coded with NotFound when nothing matches or
// when a suffix matches more than one owner.
func (c *Config) ResolveOwner(name string) (*Owner, error) {
	if o := c.FindOwnerByName(name); o != nil {
		return o, nil
	}
	var matches []*Owner
	var candidates []string
	for i := range c.Owners {
		n, err := c.Owners[i].ResolvedName()
		if err != nil {
			continue
		}
		if strings.HasSuffix(n, "/"+name) {
			matches = append(matches, &c.Owners[i])
			candidates = append(candidates, n)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, errs.New(errs.NotFound, "owner %q is not in the config", name)
	default:
		return nil, errs.New(errs.NotFound,
			"owner %q is ambiguous; matches: %s", name, strings.Join(candidates, ", "))
	}
}

// EmptyTemplate returns the contents of an empty config file with a
// helpful header comment.
func EmptyTemplate() []byte {
	return []byte(`# shed config.
# Add a repo with:        shed add <repo-url>
# Add a whole user/org:   shed add <owner-url>   # needs gh
# List with:              shed ls
# Sync with:              shed sync
#
# Manual entries look like:
# [[repo]]
# url = "https://github.com/owner/name"
# # name = "owner/name"   # optional; default derived from URL (and track)
# # track = "v2-7-stable" # optional; branch or tag to follow (default: default branch)
# # git = { "user.email" = "me@work.com" }   # git config, applied to clones
#
# [[owner]]
# url = "https://github.com/owner"   # sync discovers + adds this owner's repos
# # include_forks = false
# # include_archived = false
# # visibility = "all"   # all|public|private
# # exclude = ["owner/repo"]

`)
}
