// Package forge talks to GitHub by shelling out to the `gh` CLI: it discovers
// the repos belonging to an owner (a user or org) and reports whether a
// branch has a merged pull request. This is shed's only runtime
// dependency beyond `git` — everything else syncs with plain `git`, so callers
// degrade gracefully when `gh` is missing or unauthenticated (see the sentinel
// errors below).
package forge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/AndrewHannigan/shed/pkg/paths"
)

// Sentinel errors callers use to decide how to degrade. ErrGhMissing means the
// gh binary is not installed; ErrGhUnauthed means it is installed but not
// logged in (so discovery, especially of private repos, cannot proceed).
var (
	ErrGhMissing  = errors.New("gh CLI not found on PATH (needed to expand owners)")
	ErrGhUnauthed = errors.New("gh CLI is not authenticated (run `gh auth login`)")
)

// defaultLimit caps how many repos we list per owner. gh defaults to 30, which
// silently truncates large orgs, so we ask for a generous ceiling instead.
const defaultLimit = 1000

// Filter controls which of an owner's repos discovery returns. The zero value
// means: exclude forks, exclude archived, all visibilities.
type Filter struct {
	IncludeForks    bool
	IncludeArchived bool
	Visibility      string // "", "all", "public", or "private"
	Limit           int    // <= 0 means defaultLimit
}

// Repo is one repo discovered under an owner.
type Repo struct {
	Name       string // short name, no owner prefix
	CloneURL   string // chosen to match the owner URL's protocol (https vs ssh)
	IsFork     bool
	IsArchived bool
	Visibility string
}

// ghRepo mirrors the JSON object `gh repo list --json ...` emits.
type ghRepo struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	SSHURL     string `json:"sshUrl"`
	IsFork     bool   `json:"isFork"`
	IsArchived bool   `json:"isArchived"`
	Visibility string `json:"visibility"`
}

// Available reports whether gh is usable: installed and authenticated. It
// returns ErrGhMissing or ErrGhUnauthed so callers can warn precisely, or nil
// when gh is ready. Used by `add` to warn early; ListOwnerRepos does its
// own check so it never lists twice.
func Available() error {
	if _, err := exec.LookPath("gh"); err != nil {
		return ErrGhMissing
	}
	if out, err := exec.Command("gh", "auth", "status").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", ErrGhUnauthed, strings.TrimSpace(string(out)))
	}
	return nil
}

// ListOwnerRepos lists the repos under ownerURL (e.g.
// "https://github.com/octocat") subject to f. On success it returns one
// entry per repo with a clone URL matching ownerURL's protocol. It returns
// ErrGhMissing / ErrGhUnauthed (wrapped) when gh can't be used, so the caller
// can skip this owner and continue syncing already-known repos.
func ListOwnerRepos(ownerURL string, f Filter) ([]Repo, error) {
	host, login, err := paths.ParseURL(ownerURL)
	if err != nil {
		return nil, err
	}
	if strings.Contains(login, "/") {
		return nil, fmt.Errorf("%q is a repo URL, not an owner URL", ownerURL)
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, ErrGhMissing
	}

	cmd := exec.Command("gh", buildListArgs(login, f)...)
	// Target enterprise hosts by setting GH_HOST; github.com is gh's default.
	if host != "" && host != "github.com" {
		cmd.Env = append(os.Environ(), "GH_HOST="+host)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, classifyExecErr("gh repo list", err, stderr.String())
	}
	return decodeRepos(out, isSSHURL(ownerURL))
}

// OwnerExists reports whether ownerURL names a real GitHub user or
// organization. It shells out to `gh api users/<login>`, whose /users/<login>
// endpoint resolves both users and orgs, so a 200 means the owner exists and a
// 404 means it does not. It returns ErrGhMissing / ErrGhUnauthed (wrapped) when
// gh can't be used, so the caller can decide whether to proceed without the
// check. An owner that exists but currently has no repos still reports true —
// distinguishing "doesn't exist" from "exists but empty" is exactly why this
// uses the user endpoint rather than `repo list`, which can't tell them apart.
func OwnerExists(ownerURL string) (bool, error) {
	host, login, err := paths.ParseURL(ownerURL)
	if err != nil {
		return false, err
	}
	if strings.Contains(login, "/") {
		return false, fmt.Errorf("%q is a repo URL, not an owner URL", ownerURL)
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return false, ErrGhMissing
	}
	cmd := exec.Command("gh", buildOwnerCheckArgs(login)...)
	if host != "" && host != "github.com" {
		cmd.Env = append(os.Environ(), "GH_HOST="+host)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr // Stdout is left nil so the response body is discarded.
	return classifyOwnerCheck(cmd.Run(), stderr.String())
}

// buildOwnerCheckArgs builds the `gh api` argument vector that fetches the
// account named login. Kept pure (no exec) so it can be unit-tested.
func buildOwnerCheckArgs(login string) []string {
	// "users/"+login can never begin with "-", so it can't be read as a flag
	// even when login itself starts with a dash (argument injection).
	return []string{"api", "users/" + login}
}

// classifyOwnerCheck interprets the result of `gh api users/<login>`: a nil err
// means the owner exists; a 404 in stderr means it does not (false, nil); auth
// and missing-binary failures surface as sentinels and anything else as a real
// error, so a transient failure is never mistaken for "does not exist". Pure
// for testability.
func classifyOwnerCheck(runErr error, stderr string) (bool, error) {
	if runErr == nil {
		return true, nil
	}
	s := strings.ToLower(stderr)
	if strings.Contains(s, "http 404") || strings.Contains(s, "not found") {
		return false, nil
	}
	return false, classifyExecErr("gh api", runErr, stderr)
}

// MergedPR returns the number of a merged pull request whose head branch is
// branch in repo (an "owner/name" slug), or 0 if there is none. host selects
// the GitHub host: "" or "github.com" use gh's default; any other value
// targets an enterprise host via GH_HOST. Like ListOwnerRepos it returns
// ErrGhMissing / ErrGhUnauthed (wrapped) when gh can't be used.
func MergedPR(host, repo, branch string) (int, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return 0, ErrGhMissing
	}
	cmd := exec.Command("gh", buildPRListArgs(repo, branch)...)
	if host != "" && host != "github.com" {
		cmd.Env = append(os.Environ(), "GH_HOST="+host)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, classifyExecErr("gh pr list", err, stderr.String())
	}
	return decodeMergedPR(out)
}

// buildPRListArgs builds the `gh pr list` argument vector that asks for the
// single newest merged PR whose head branch is branch. Kept pure (no exec) so
// it can be unit-tested.
func buildPRListArgs(repo, branch string) []string {
	// The "--flag=value" form binds each value to its flag, so a repo or branch
	// beginning with "-" can't be mistaken for a separate flag.
	return []string{
		"pr", "list",
		"--repo=" + repo,
		"--head=" + branch,
		"--state", "merged",
		"--json", "number",
		"--limit", "1",
	}
}

// decodeMergedPR parses the JSON array gh emits for `pr list` and returns the
// first PR's number, or 0 when the array is empty. Pure for testability.
func decodeMergedPR(data []byte) (int, error) {
	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(data, &prs); err != nil {
		return 0, fmt.Errorf("parse gh pr list output: %w", err)
	}
	if len(prs) == 0 {
		return 0, nil
	}
	return prs[0].Number, nil
}

// PR is the metadata `workspace from-pr` needs about a pull request: which
// branch to check out, whether the head lives in a fork, and enough state to
// warn about a PR that is no longer open.
type PR struct {
	Number      int
	Title       string
	State       string // "OPEN", "MERGED", or "CLOSED"
	HeadRefName string // the PR's head branch (in the head repo)
	CrossRepo   bool   // true when the head branch lives in a fork
	HeadOwner   string // owner of the head repo ("" when unknown)
	HeadName    string // name of the head repo ("" when unknown)
}

// ViewPR fetches the metadata for pull request number in repo (an
// "owner/name" slug). host selects the GitHub host exactly as in MergedPR.
// Like the other forge calls it returns ErrGhMissing / ErrGhUnauthed
// (wrapped) when gh can't be used, so callers can degrade.
func ViewPR(host, repo string, number int) (PR, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return PR{}, ErrGhMissing
	}
	cmd := exec.Command("gh", buildPRViewArgs(repo, number)...)
	if host != "" && host != "github.com" {
		cmd.Env = append(os.Environ(), "GH_HOST="+host)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return PR{}, classifyExecErr("gh pr view", err, stderr.String())
	}
	return decodePR(out)
}

// buildPRViewArgs builds the `gh pr view` argument vector that fetches the
// fields PR carries. Kept pure (no exec) so it can be unit-tested.
func buildPRViewArgs(repo string, number int) []string {
	// number is formatted from an int so it can never read as a flag; the
	// "--repo=" form binds the slug to its flag (argument injection).
	return []string{
		"pr", "view", strconv.Itoa(number),
		"--repo=" + repo,
		"--json", "number,title,state,headRefName,isCrossRepository,headRepository,headRepositoryOwner",
	}
}

// ghPR mirrors the JSON object `gh pr view --json ...` emits.
type ghPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	CrossRepo   bool   `json:"isCrossRepository"`
	HeadRepo    struct {
		Name string `json:"name"`
	} `json:"headRepository"`
	HeadOwner struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
}

// decodePR parses the JSON object gh emits for `pr view`. Pure for
// testability.
func decodePR(data []byte) (PR, error) {
	var g ghPR
	if err := json.Unmarshal(data, &g); err != nil {
		return PR{}, fmt.Errorf("parse gh pr view output: %w", err)
	}
	return PR{
		Number:      g.Number,
		Title:       g.Title,
		State:       g.State,
		HeadRefName: g.HeadRefName,
		CrossRepo:   g.CrossRepo,
		HeadOwner:   g.HeadOwner.Login,
		HeadName:    g.HeadRepo.Name,
	}, nil
}

// ForkCloneURL builds a clone URL for the fork at host/owner/name, matching
// baseURL's transport — the workspace could authenticate to the base repo
// with that transport, so the fork gets the same one. Pure for testability.
func ForkCloneURL(baseURL, host, owner, name string) string {
	if isSSHURL(baseURL) {
		return "git@" + host + ":" + owner + "/" + name + ".git"
	}
	return "https://" + host + "/" + owner + "/" + name
}

// buildListArgs builds the `gh repo list` argument vector for login under f.
// Kept pure (no exec) so it can be unit-tested.
func buildListArgs(login string, f Filter) []string {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	args := []string{
		"repo", "list",
		"--limit", strconv.Itoa(limit),
		"--json", "name,url,sshUrl,isFork,isArchived,visibility",
	}
	if !f.IncludeForks {
		args = append(args, "--source") // sources only (non-forks)
	}
	if !f.IncludeArchived {
		args = append(args, "--no-archived")
	}
	if v := strings.ToLower(f.Visibility); v != "" && v != "all" {
		args = append(args, "--visibility", v)
	}
	// "--" terminates flags, then login positionally, so an owner that begins
	// with "-" can't be parsed as a gh flag (argument injection).
	return append(args, "--", login)
}

// decodeRepos parses the JSON array gh emits and maps each entry to a Repo,
// selecting the ssh or https clone URL per wantSSH. Pure for testability.
func decodeRepos(data []byte, wantSSH bool) ([]Repo, error) {
	var ghRepos []ghRepo
	if err := json.Unmarshal(data, &ghRepos); err != nil {
		return nil, fmt.Errorf("parse gh repo list output: %w", err)
	}
	repos := make([]Repo, 0, len(ghRepos))
	for _, g := range ghRepos {
		cloneURL := g.URL
		if wantSSH && g.SSHURL != "" {
			cloneURL = g.SSHURL
		}
		repos = append(repos, Repo{
			Name:       g.Name,
			CloneURL:   cloneURL,
			IsFork:     g.IsFork,
			IsArchived: g.IsArchived,
			Visibility: g.Visibility,
		})
	}
	return repos, nil
}

// classifyExecErr turns a failed gh invocation (what names it, e.g.
// "gh repo list") into a sentinel where possible so callers can degrade.
// Pure for testability.
func classifyExecErr(what string, err error, stderr string) error {
	if errors.Is(err, exec.ErrNotFound) {
		return ErrGhMissing
	}
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "not logged in"),
		strings.Contains(s, "gh auth login"),
		strings.Contains(s, "authentication"),
		strings.Contains(s, "requires authentication"):
		return fmt.Errorf("%w: %s", ErrGhUnauthed, strings.TrimSpace(stderr))
	default:
		return fmt.Errorf("%s failed: %v: %s", what, err, strings.TrimSpace(stderr))
	}
}

// isSSHURL reports whether a git URL uses SSH (scp-style git@host:... or an
// ssh:// scheme), so discovered repos get matching clone URLs.
func isSSHURL(rawURL string) bool {
	return strings.HasPrefix(rawURL, "git@") || strings.HasPrefix(rawURL, "ssh://")
}
