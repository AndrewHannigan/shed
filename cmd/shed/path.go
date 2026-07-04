package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/catalog"
	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

func newPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path <name>",
		Short: "Print the absolute path of a repo or workspace by name",
		Long: `path prints the absolute path of a stored repo or a workspace,
located by a single name — and nothing else. It changes no directory of its own;
compose it yourself with cd:

    cd "$(shed path projects)"      # a repo (read-only store)
    cd "$(shed path my-workspace)"  # a workspace (writable)

Repo names and workspace names share one namespace — a workspace can never have
the same name as a repo, and a repo can never have the same name as a workspace
(enforced when each is created) — so one name resolves to exactly one path. A
repo matches by exact name or an unambiguous trailing path segment (so
"projects" finds "github.com/you/projects").

Two repos may share a leaf name under different owners (e.g.
"github.com/alice/projects" and "github.com/bob/projects") — that's allowed. A
bare "projects" is then ambiguous and errors; pass the owner/repo form
("alice/projects"), or the full name, to choose one.

Exits 2 if the name matches nothing, matches more than one repo, or matches a
repo that isn't synced yet.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPath(args[0])
		},
	}
}

func runPath(name string) error {
	c, err := config.Load()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}

	_, wsPath, wsFound := workspace.LocateByName(repoNames(c), name)
	repoMatches := repoNamesMatching(c, name)

	// Repo and workspace names share one namespace (enforced at creation by the
	// guards in `add` and `workspace new`), so a healthy library has at most one
	// match. If somehow both match — a library that predates the guards — refuse
	// rather than silently pick one.
	if wsFound && len(repoMatches) > 0 {
		return errs.New(errs.Exists,
			"%q matches both a workspace and the repo %s; rename one (see `shed ls`)", name, repoMatches[0])
	}

	switch {
	case wsFound:
		fmt.Println(wsPath)
		return nil
	case len(repoMatches) == 1:
		repoName := repoMatches[0]
		if !catalog.Exists(repoName) {
			return errs.New(errs.NotFound,
				"repo %s is not synced yet; run `shed sync %s` first", repoName, name)
		}
		fmt.Println(catalog.Path(repoName))
		return nil
	case len(repoMatches) > 1:
		// Several repos share this leaf (same name under different owners/hosts,
		// which is allowed). The bare name can't pick one — ask for the more
		// specific owner/repo (or full) form, which resolves by the same rule.
		return errs.New(errs.NotFound,
			"%q matches several repos; pass owner/repo (or the full name) to choose one: %s",
			name, strings.Join(repoMatches, ", "))
	default:
		return errs.New(errs.NotFound, "no repo or workspace named %q (see `shed ls`)", name)
	}
}

// repoNamesMatching returns the resolved names of every stored repo that the
// name n selects under `shed path` — the same exact-then-trailing-"/"-segment
// rule config.Resolve uses. More than one match means n is an ambiguous repo
// reference. Shared by the conflict guards in `add` and `workspace new` so the
// notion of "this name already belongs to a repo" stays identical to what
// `path` resolves.
func repoNamesMatching(c *config.Config, n string) []string {
	var out []string
	for i := range c.Repos {
		rn, err := c.Repos[i].ResolvedName()
		if err != nil {
			continue
		}
		if rn == n || strings.HasSuffix(rn, "/"+n) {
			out = append(out, rn)
		}
	}
	return out
}

// workspaceNamesShadowedBy returns existing workspace names that would resolve
// to the repo repoName under `shed path` — i.e. names that, once a repo named
// repoName exists, would be ambiguous between the workspace and the repo. It is
// the `add`-side mirror of repoNamesMatching: there a candidate workspace name
// is tested against existing repos; here a candidate repo name is tested against
// existing workspaces.
func workspaceNamesShadowedBy(c *config.Config, repoName string) []string {
	infos, err := workspace.List(repoNames(c))
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range infos {
		w := i.Branch // the workspace name
		if repoName == w || strings.HasSuffix(repoName, "/"+w) {
			out = append(out, w)
		}
	}
	return out
}
