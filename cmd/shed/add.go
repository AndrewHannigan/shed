package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/forge"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

// configLockTimeout bounds how long a config-mutating command waits for the
// config (and per-repo store) lock before giving up. Shared by add, rm, and
// sync.
const configLockTimeout = 2 * time.Second

func newAddCmd() *cobra.Command {
	var name, track string
	var asOwner, asRepo bool
	cmd := &cobra.Command{
		Use:   "add <repo>",
		Short: "Add a repository, or a whole user/org, to the library",
		Long: `add appends a repo to the library. <repo> may be a full git URL
(https://, ssh://, or scp-style git@host:owner/repo) or GitHub shorthand:
a bare "owner/repo" or "owner" is expanded against github.com, so
"shed add octocat/Hello-World" and "shed add octocat"
both just work.

--track pins the checkout to a branch or tag instead of the default branch:
a branch advances on every sync, a tag never changes. Several versions of one
repo can be tracked side by side ("shed add apache/airflow --track
v2-7-stable" lives next to plain apache/airflow); they share one mirror on
disk, so extra versions cost a checkout, not another copy of history. The
name gains an "@<track>" suffix (airflow@v2-7-stable). Bare track names
prefer a branch over a same-named tag; pin with "heads/<n>" or "tags/<n>".

If <repo> points at a bare user or org (a single path segment, e.g.
octocat or https://github.com/octocat), it is tracked as an owner instead:
each sync discovers that owner's repos via gh and adds any new ones
automatically. The owner is checked against GitHub first and rejected if no
such user or org exists, so a typo can't become a dead entry that syncs
nothing.

Detection is automatic from the shape; force it with --owner / --repo.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoAdd(args[0], name, track, asOwner, asRepo)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "override the default name (derived from URL and track)")
	cmd.Flags().StringVar(&track, "track", "", "branch or tag to follow (default: the upstream default branch)")
	cmd.Flags().BoolVar(&asOwner, "owner", false, "track <repo> as a user/org (discover its repos via gh)")
	cmd.Flags().BoolVar(&asRepo, "repo", false, "track <repo> as a single repo (default for owner/repo references)")
	return cmd
}

func runRepoAdd(input, overrideName, track string, asOwner, asRepo bool) error {
	if asOwner && asRepo {
		return errs.New(errs.Config, "--owner and --repo are mutually exclusive")
	}
	if track != "" {
		if err := paths.ValidateTrack(track); err != nil {
			return errs.Wrap(errs.Config, err)
		}
	}
	// Expand shorthand (e.g. "octocat" or "owner/repo") into a full
	// URL up front so classification, naming, and the stored config entry all
	// use the same canonical form.
	url := paths.NormalizeURL(input)
	isOwner := asOwner
	if !asOwner && !asRepo {
		detected, err := paths.IsOwnerURL(url)
		if err != nil {
			return errs.Wrap(errs.Config, err)
		}
		isOwner = detected
	}
	if isOwner {
		if track != "" {
			return errs.New(errs.Config, "--track applies to a single repo, not an owner")
		}
		return runOwnerAdd(url, overrideName)
	}
	return runRepoAddOne(url, overrideName, track)
}

func runRepoAddOne(url, overrideName, track string) error {
	// Validate URL and derive default name up front so we fail before locking.
	defaultName, err := paths.DefaultRepoName(url, track)
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	// Preflight auth while the user is here interactively. If the chosen
	// transport can't authenticate but the other one can, store the working
	// URL instead — this is the common "shorthand expands to HTTPS but I only
	// have SSH set up" case. The name is transport-independent, so swapping the
	// URL never changes effectiveName below.
	url = reachableURL(url)
	effectiveName := defaultName
	if overrideName != "" {
		effectiveName = overrideName
	}

	err = config.WithLock(configLockTimeout, func(c *config.Config) error {
		if c.FindByName(effectiveName) != nil {
			return errs.New(errs.Exists, "repo %q is already in the config", effectiveName)
		}
		if c.FindOwnerByName(effectiveName) != nil {
			return errs.New(errs.Exists, "%q is already tracked as an owner", effectiveName)
		}
		// Repo and workspace names share one namespace so `shed path <name>` is
		// unambiguous. Reject a repo whose name would resolve to an existing
		// workspace (rename the workspace, or pass a distinct --name).
		if ws := workspaceNamesShadowedBy(c, effectiveName); len(ws) > 0 {
			return errs.New(errs.Exists,
				"repo %s collides with workspace %q; rename the workspace or pass --name so `shed path %s` is unambiguous",
				effectiveName, ws[0], ws[0])
		}
		c.Repos = append(c.Repos, config.Repo{URL: url, Name: overrideName, Track: track})
		return config.Save(c)
	})
	if err != nil {
		if errors.Is(err, config.ErrLocked) {
			return errs.Wrap(errs.Locked, err)
		}
		return errs.EnsureCoded(err, errs.Config)
	}
	fmt.Printf("added %s\n", effectiveName)
	// Fetch the new repo right away so the store is populated without a
	// separate `shed sync`. Scoped to just this repo.
	return runSync([]string{effectiveName}, syncDefaultJobs, 0, false)
}

func runOwnerAdd(url, overrideName string) error {
	defaultName, err := paths.DefaultOwnerName(url)
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	effectiveName := defaultName
	if overrideName != "" {
		effectiveName = overrideName
	}

	// Validate the owner against the forge before committing it to the config.
	// A name that resolves to no GitHub account — a typo, or an org whose login
	// differs from its display name ("langchain" for the org "langchain-ai") —
	// would otherwise linger as a dead config entry that silently syncs nothing
	// on every pass, so refuse it outright. When gh is unavailable we can't
	// look; save anyway (the entry will expand once gh returns) and warn below.
	// An owner that exists but has no repos is allowed through and warned about
	// after sync — see ownerEmptyHint.
	ghErr := forge.Available()
	if ghErr == nil {
		exists, err := forge.OwnerExists(url)
		if err != nil {
			return errs.Wrap(errs.Config, err)
		}
		if !exists {
			return errs.New(errs.NotFound,
				"%s is not a user or organization on GitHub — check the spelling "+
					"(an org's GitHub login can differ from its display name)", effectiveName)
		}
	}

	err = config.WithLock(configLockTimeout, func(c *config.Config) error {
		if c.FindOwnerByName(effectiveName) != nil {
			return errs.New(errs.Exists, "owner %q is already in the config", effectiveName)
		}
		if c.FindByName(effectiveName) != nil {
			return errs.New(errs.Exists, "%q is already tracked as a repo", effectiveName)
		}
		c.Owners = append(c.Owners, config.Owner{URL: url, Name: overrideName})
		return config.Save(c)
	})
	if err != nil {
		if errors.Is(err, config.ErrLocked) {
			return errs.Wrap(errs.Locked, err)
		}
		return errs.EnsureCoded(err, errs.Config)
	}

	fmt.Printf("added owner %s\n", effectiveName)
	if ghErr != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n  owner expansion will be skipped until gh is available and authenticated.\n", ghErr)
	}
	// Discover and fetch the owner's repos right away. Scoped to this owner,
	// runSync reconciles it (adding newly discovered repos) and syncs them.
	syncErr := runSync([]string{effectiveName}, syncDefaultJobs, 0, false)
	// The owner is known to exist (validated above), so zero repos here means it
	// is genuinely empty — nothing to sync but not a mistake to refuse. Point at
	// `shed rm` in case that's unexpected. Skipped when gh is unavailable: 0
	// there means "couldn't look", not "no repos", already warned about above.
	if ghErr == nil {
		if c, err := config.Load(); err == nil {
			if hint := ownerEmptyHint(c, effectiveName); hint != "" {
				fmt.Fprint(os.Stderr, hint)
			}
		}
	}
	return syncErr
}

// ownerEmptyHint returns an advisory (or "" when none is warranted) for an
// owner that, just after being added and synced, manages no repos at all. The
// owner is already known to exist (add validates that before saving), so this
// is the benign "exists but empty" case — nothing to sync now, though new repos
// will be picked up on a later sync. It still points at `shed rm` in case the
// emptiness is unexpected. Pure (it decides from the config alone) so both the
// trigger and the wording are testable without gh or disk.
func ownerEmptyHint(c *config.Config, ownerName string) string {
	if len(c.ReposForOwner(ownerName)) > 0 {
		return ""
	}
	return fmt.Sprintf("warning: owner %s has no repos to sync yet.\n"+
		"  Remove it with `shed rm %s` if that's unexpected.\n", ownerName, ownerName)
}

// reachableURL returns a clone URL that authenticates with the user's current
// setup, given the one they asked for. If url works as-is (the common case,
// including public repos that need no auth) it is returned unchanged. If url
// fails specifically on authentication and the other transport (SSH↔HTTPS)
// works, the working URL is returned with a note. Otherwise url is returned
// unchanged — add never blocks: the entry is saved regardless and the
// subsequent sync reports any remaining problem with a protocol-aware fix.
func reachableURL(url string) string {
	if gitx.RequireGit() != nil {
		return url // no git to probe with; sync will report it.
	}
	err := gitx.Reachable(url)
	if err == nil {
		return url
	}
	// Only a credential failure is worth switching transports for. Network or
	// not-found errors would fail the same way over either protocol.
	if !isAuthError(strings.ToLower(err.Error())) {
		return url
	}
	if alt := paths.AlternateProtocolURL(url); alt != "" && gitx.Reachable(alt) == nil {
		fmt.Printf("note: %s could not authenticate, but %s works — adding it over %s instead.\n",
			url, alt, transportLabel(alt))
		return alt
	}
	fmt.Fprintf(os.Stderr, "warning: could not authenticate to %s.\n  %s\n  Adding it anyway; fix auth and run `shed sync`.\n",
		url, authFixHint(url))
	return url
}

// transportLabel names the transport a URL uses, for human-facing notes.
func transportLabel(url string) string {
	if paths.IsSSHURL(url) {
		return "SSH"
	}
	return "HTTPS"
}
