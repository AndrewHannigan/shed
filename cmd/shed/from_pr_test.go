package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/forge"
	"github.com/AndrewHannigan/shed/pkg/gitx"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

func TestParsePRRef(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		token   string
		number  int
		wantErr bool
	}{
		{"github URL", "https://github.com/octocat/Hello-World/pull/42", "github.com/octocat/Hello-World", 42, false},
		{"URL with trailing segment", "https://github.com/octocat/Hello-World/pull/42/files", "github.com/octocat/Hello-World", 42, false},
		{"URL with fragment", "https://github.com/octocat/Hello-World/pull/42#discussion_r1", "github.com/octocat/Hello-World", 42, false},
		{"enterprise host URL", "https://ghe.acme.com/team/widgets/pull/7", "ghe.acme.com/team/widgets", 7, false},
		{"owner/repo#n", "octocat/Hello-World#42", "octocat/Hello-World", 42, false},
		{"repo#n", "Hello-World#42", "Hello-World", 42, false},
		{"issue URL is not a PR", "https://github.com/octocat/Hello-World/issues/42", "", 0, true},
		{"URL without number", "https://github.com/octocat/Hello-World/pull/", "", 0, true},
		{"no separator", "Hello-World", "", 0, true},
		{"empty repo before #", "#42", "", 0, true},
		{"non-numeric after #", "Hello-World#abc", "", 0, true},
		{"negative number", "Hello-World#-2", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, number, err := parsePRRef(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePRRef(%q) = (%q, %d), want error", tt.in, token, number)
				}
				return
			}
			if err != nil || token != tt.token || number != tt.number {
				t.Fatalf("parsePRRef(%q) = (%q, %d, %v), want (%q, %d, nil)",
					tt.in, token, number, err, tt.token, tt.number)
			}
		})
	}
}

func TestAddSuggestion(t *testing.T) {
	if got := addSuggestion("github.com/octocat/Hello-World"); got != "octocat/Hello-World" {
		t.Errorf("addSuggestion(github.com/...) = %q, want gh shorthand", got)
	}
	if got := addSuggestion("ghe.acme.com/team/widgets"); got != "ghe.acme.com/team/widgets" {
		t.Errorf("addSuggestion(enterprise) = %q, want unchanged", got)
	}
	// `shed add <one-segment>` means an owner, so a bare repo token must not
	// be suggested verbatim — it gains an <owner>/ placeholder.
	if got := addSuggestion("Hello-World"); got != "<owner>/Hello-World" {
		t.Errorf("addSuggestion(bare) = %q, want <owner>/Hello-World", got)
	}
}

// ghSlugForRepo must derive host/slug from the URL, not the config name — a
// custom `name` override would parse into the wrong host (or not at all).
func TestGhSlugForRepo(t *testing.T) {
	repo := &config.Repo{URL: "https://github.com/acme/widget", Name: "corp/tools/widget"}
	host, slug, ok := ghSlugForRepo(repo, "corp/tools/widget")
	if !ok || host != "github.com" || slug != "acme/widget" {
		t.Fatalf("ghSlugForRepo = (%q, %q, %v), want (github.com, acme/widget, true)", host, slug, ok)
	}
}

// A PR URL must resolve a repo tracked only under pinned versions: the URL
// names the upstream, which every @track entry shares.
func TestResolvePRRepoTrackPinned(t *testing.T) {
	c := &config.Config{Repos: []config.Repo{
		{URL: "https://github.com/apache/airflow", Track: "v2-7-stable"},
	}}
	repo, err := resolvePRRepo(c, "github.com/apache/airflow")
	if err != nil || repo == nil || repo.Track != "v2-7-stable" {
		t.Fatalf("resolvePRRepo = (%+v, %v), want the pinned entry", repo, err)
	}

	// With a default-track entry alongside, prefer it.
	c.Repos = append(c.Repos, config.Repo{URL: "https://github.com/apache/airflow"})
	repo, err = resolvePRRepo(c, "github.com/apache/airflow")
	if err != nil || repo == nil || repo.Track != "" {
		t.Fatalf("resolvePRRepo = (%+v, %v), want the default-track entry", repo, err)
	}
}

func TestFromPRBranch(t *testing.T) {
	openPR := forge.PR{Number: 7, HeadRefName: "fix-flux", State: "OPEN"}

	t.Run("head branch is the default name", func(t *testing.T) {
		branch, warnings, err := fromPRBranch(openPR, nil, "", 7)
		if err != nil || branch != "fix-flux" {
			t.Fatalf("= (%q, %v), want fix-flux", branch, err)
		}
		if len(warnings) != 0 {
			t.Fatalf("unexpected warnings: %v", warnings)
		}
	})

	t.Run("--name wins", func(t *testing.T) {
		branch, _, err := fromPRBranch(openPR, nil, "my-review", 7)
		if err != nil || branch != "my-review" {
			t.Fatalf("= (%q, %v), want my-review", branch, err)
		}
	})

	t.Run("no gh falls back to pr-<n> and warns", func(t *testing.T) {
		branch, warnings, err := fromPRBranch(forge.PR{}, forge.ErrGhMissing, "", 7)
		if err != nil || branch != "pr-7" {
			t.Fatalf("= (%q, %v), want pr-7", branch, err)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "without PR metadata") {
			t.Fatalf("warnings = %v, want a degraded-mode warning", warnings)
		}
	})

	t.Run("merged PR warns but proceeds", func(t *testing.T) {
		merged := openPR
		merged.State = "MERGED"
		branch, warnings, err := fromPRBranch(merged, nil, "", 7)
		if err != nil || branch != "fix-flux" {
			t.Fatalf("= (%q, %v), want fix-flux", branch, err)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "merged") {
			t.Fatalf("warnings = %v, want a merged-state warning", warnings)
		}
	})

	t.Run("unsafe head branch needs --name", func(t *testing.T) {
		bad := forge.PR{Number: 7, HeadRefName: "../escape", State: "OPEN"}
		if _, _, err := fromPRBranch(bad, nil, "", 7); err == nil {
			t.Fatal("want an error steering to --name")
		}
		branch, _, err := fromPRBranch(bad, nil, "safe", 7)
		if err != nil || branch != "safe" {
			t.Fatalf("with --name = (%q, %v), want safe", branch, err)
		}
	})
}

func TestPlanPRCheckout(t *testing.T) {
	samePR := forge.PR{Number: 7, HeadRefName: "fix-flux", State: "OPEN"}
	forkPR := forge.PR{Number: 9, HeadRefName: "fix-1", State: "OPEN",
		CrossRepo: true, HeadOwner: "contrib", HeadName: "widget"}
	fresh := prRefState{synced: true, headInMirror: true}
	const url = "https://github.com/acme/widget"

	t.Run("same-repo head in fresh mirror is a plain checkout", func(t *testing.T) {
		co := planPRCheckout(samePR, nil, fresh, "fix-flux", url, "github.com")
		if co != (prCheckout{}) {
			t.Fatalf("= %+v, want the zero plan", co)
		}
	})

	t.Run("same-repo head missing from mirror falls back to the pull ref", func(t *testing.T) {
		co := planPRCheckout(samePR, nil, prRefState{synced: true}, "fix-flux", url, "github.com")
		if !co.pullFetch || co.forkURL != "" {
			t.Fatalf("= %+v, want pullFetch without a fork", co)
		}
	})

	t.Run("degraded sync distrusts the mirror branch tip", func(t *testing.T) {
		// The branch exists in the mirror but the refresh failed — its tip
		// may be days behind the PR, so only the pull ref is authoritative.
		co := planPRCheckout(samePR, nil, prRefState{headInMirror: true}, "fix-flux", url, "github.com")
		if !co.pullFetch {
			t.Fatalf("= %+v, want pullFetch when the sync degraded", co)
		}
	})

	t.Run("merged PR goes through the frozen pull ref", func(t *testing.T) {
		// A merged PR's head branch may have moved on; refs/pull/<n>/head is
		// the PR's actual final state.
		merged := samePR
		merged.State = "MERGED"
		co := planPRCheckout(merged, nil, fresh, "fix-flux", url, "github.com")
		if !co.pullFetch {
			t.Fatalf("= %+v, want pullFetch for a non-open PR", co)
		}
	})

	t.Run("--name override bases on the head branch, still offline", func(t *testing.T) {
		co := planPRCheckout(samePR, nil, fresh, "my-review", url, "github.com")
		if co.pullFetch || co.base != "fix-flux" {
			t.Fatalf("= %+v, want base=fix-flux without pullFetch", co)
		}
	})

	t.Run("--name override colliding with an upstream branch uses the pull ref", func(t *testing.T) {
		// workspace.New prefers an upstream branch named like the override to
		// any base; the hard reset makes the tip right regardless.
		taken := fresh
		taken.branchTaken = true
		co := planPRCheckout(samePR, nil, taken, "main", url, "github.com")
		if !co.pullFetch {
			t.Fatalf("= %+v, want pullFetch", co)
		}
	})

	t.Run("fork PR gets a fork remote with tracking", func(t *testing.T) {
		co := planPRCheckout(forkPR, nil, prRefState{synced: true}, "fix-1", url, "github.com")
		want := prCheckout{pullFetch: true, forkURL: "https://github.com/contrib/widget", trackRef: "fix-1"}
		if co != want {
			t.Fatalf("= %+v, want %+v", co, want)
		}
	})

	t.Run("fork PR with renamed branch skips tracking", func(t *testing.T) {
		co := planPRCheckout(forkPR, nil, prRefState{synced: true}, "my-review", url, "github.com")
		if co.trackRef != "" || co.forkURL == "" {
			t.Fatalf("= %+v, want fork remote without tracking", co)
		}
	})

	t.Run("deleted fork has no fork remote", func(t *testing.T) {
		gone := forkPR
		gone.HeadOwner, gone.HeadName = "", ""
		co := planPRCheckout(gone, nil, prRefState{synced: true}, "fix-1", url, "github.com")
		if !co.pullFetch || co.forkURL != "" {
			t.Fatalf("= %+v, want pullFetch without a fork", co)
		}
	})

	t.Run("no gh is the pull ref alone", func(t *testing.T) {
		co := planPRCheckout(forge.PR{}, forge.ErrGhMissing, prRefState{synced: true}, "pr-7", url, "github.com")
		if !co.pullFetch || co.forkURL != "" || co.base != "" {
			t.Fatalf("= %+v, want bare pullFetch", co)
		}
	})
}

// --- end-to-end tests -------------------------------------------------------
//
// The full command runs against local git repos standing in for GitHub: a
// url.<path>.insteadOf mapping in the temp HOME's ~/.gitconfig rewrites the
// config's https://github.com/... URLs to the local upstreams for every git
// command shed runs (sync's mirror fetch, the workspace's PR fetch), while
// shed itself only ever sees the real-looking URLs. gh is stubbed via the
// prView seam.

type fromPREnv struct {
	upstream   string // local upstream standing in for github.com/acme/widget
	fork       string // local repo standing in for github.com/contrib/widget
	mainSHA    string
	featureSHA string // tip of upstream branch feature-x, published as refs/pull/7/head
	forkSHA    string // tip of fork branch fix-1, published as refs/pull/9/head
}

const (
	fromPRRepoName = "github.com/acme/widget"
	fromPRRepoURL  = "https://github.com/acme/widget"
	fromPRForkURL  = "https://github.com/contrib/widget"
)

func setupFromPREnv(t *testing.T) fromPREnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tempHome(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := t.TempDir()
	env := fromPREnv{
		upstream: filepath.Join(root, "upstream"),
		fork:     filepath.Join(root, "fork"),
	}

	// Upstream: main, plus a PR head branch feature-x also published as
	// refs/pull/7/head (how GitHub exposes a same-repo PR).
	gitRun(t, root, "init", "-q", "-b", "main", env.upstream)
	writeUpstreamFile(t, env.upstream, "a.txt", "1")
	gitRun(t, env.upstream, "add", "a.txt")
	gitRun(t, env.upstream, "commit", "-q", "-m", "base")
	env.mainSHA = revParseT(t, env.upstream, "HEAD")

	gitRun(t, env.upstream, "checkout", "-q", "-b", "feature-x")
	writeUpstreamFile(t, env.upstream, "feature.txt", "feature")
	gitRun(t, env.upstream, "add", "feature.txt")
	gitRun(t, env.upstream, "commit", "-q", "-m", "feature work")
	env.featureSHA = revParseT(t, env.upstream, "HEAD")
	gitRun(t, env.upstream, "update-ref", "refs/pull/7/head", env.featureSHA)
	gitRun(t, env.upstream, "checkout", "-q", "main")

	// Fork: shares upstream history, adds fix-1. Its head commit is also
	// published on the upstream as refs/pull/9/head (how GitHub exposes a
	// cross-repo PR).
	gitRun(t, root, "clone", "-q", env.upstream, env.fork)
	gitRun(t, env.fork, "checkout", "-q", "-b", "fix-1")
	writeUpstreamFile(t, env.fork, "fix.txt", "fix")
	gitRun(t, env.fork, "add", "fix.txt")
	gitRun(t, env.fork, "commit", "-q", "-m", "fork fix")
	env.forkSHA = revParseT(t, env.fork, "HEAD")
	gitRun(t, env.upstream, "fetch", "-q", env.fork, "refs/heads/fix-1")
	gitRun(t, env.upstream, "update-ref", "refs/pull/9/head", env.forkSHA)

	// Map the fake GitHub URLs onto the local repos for every git command.
	gitconfig := "[url \"" + env.upstream + "\"]\n\tinsteadOf = " + fromPRRepoURL + "\n" +
		"[url \"" + env.fork + "\"]\n\tinsteadOf = " + fromPRForkURL + "\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".gitconfig"), []byte(gitconfig), 0644); err != nil {
		t.Fatal(err)
	}

	saveConfig(t, &config.Config{Repos: []config.Repo{{URL: fromPRRepoURL}}})
	return env
}

func writeUpstreamFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func revParseT(t *testing.T, dir, rev string) string {
	t.Helper()
	sha, ok := gitx.RevParse(dir, rev)
	if !ok {
		t.Fatalf("rev-parse %s in %s failed", rev, dir)
	}
	return sha
}

// stubPRView swaps the gh seam for this test.
func stubPRView(t *testing.T, fn func(host, repo string, number int) (forge.PR, error)) {
	t.Helper()
	old := prView
	prView = fn
	t.Cleanup(func() { prView = old })
}

// A same-repo PR becomes a workspace on the PR's head branch, tracking
// origin, so a plain `git push` updates the PR.
func TestRunWorkspaceFromPRSameRepo(t *testing.T) {
	env := setupFromPREnv(t)
	stubPRView(t, func(host, repo string, number int) (forge.PR, error) {
		if host != "github.com" || repo != "acme/widget" || number != 7 {
			t.Errorf("prView(%q, %q, %d), want (github.com, acme/widget, 7)", host, repo, number)
		}
		return forge.PR{Number: 7, HeadRefName: "feature-x", State: "OPEN"}, nil
	})

	captureStdout(t, func() {
		if err := runWorkspaceFromPR("https://github.com/acme/widget/pull/7", ""); err != nil {
			t.Fatalf("runWorkspaceFromPR: %v", err)
		}
	})

	ws := workspace.PathFor(fromPRRepoName, "feature-x")
	if got := revParseT(t, ws, "HEAD"); got != env.featureSHA {
		t.Fatalf("HEAD = %s, want the PR head %s", got, env.featureSHA)
	}
	if br, err := workspace.CurrentBranch(ws); err != nil || br != "feature-x" {
		t.Fatalf("branch = %q, %v; want feature-x", br, err)
	}
	out, err := exec.Command("git", "-C", ws, "rev-parse", "--abbrev-ref", "@{u}").Output()
	if err != nil || strings.TrimSpace(string(out)) != "origin/feature-x" {
		t.Fatalf("upstream = %q, %v; want origin/feature-x", strings.TrimSpace(string(out)), err)
	}
	// origin points at the real upstream URL, not shed's internals. Read the
	// raw config value: `git remote get-url` would show the insteadOf-rewritten
	// local path this test harness maps the URL to.
	if url, _ := exec.Command("git", "-C", ws, "config", "remote.origin.url").Output(); strings.TrimSpace(string(url)) != fromPRRepoURL {
		t.Fatalf("origin = %q, want %q", strings.TrimSpace(string(url)), fromPRRepoURL)
	}
}

// A cross-repo (fork) PR fetches the head via refs/pull/<n>/head and wires a
// "fork" remote the branch tracks.
func TestRunWorkspaceFromPRCrossRepo(t *testing.T) {
	env := setupFromPREnv(t)
	stubPRView(t, func(host, repo string, number int) (forge.PR, error) {
		return forge.PR{Number: 9, HeadRefName: "fix-1", State: "OPEN",
			CrossRepo: true, HeadOwner: "contrib", HeadName: "widget"}, nil
	})

	captureStdout(t, func() {
		if err := runWorkspaceFromPR("acme/widget#9", ""); err != nil {
			t.Fatalf("runWorkspaceFromPR: %v", err)
		}
	})

	ws := workspace.PathFor(fromPRRepoName, "fix-1")
	if got := revParseT(t, ws, "HEAD"); got != env.forkSHA {
		t.Fatalf("HEAD = %s, want the fork's PR head %s", got, env.forkSHA)
	}
	// Raw config value — `remote get-url` would show the insteadOf rewrite.
	if url, err := exec.Command("git", "-C", ws, "config", "remote.fork.url").Output(); err != nil || strings.TrimSpace(string(url)) != fromPRForkURL {
		t.Fatalf("fork remote = %q, %v; want %q", strings.TrimSpace(string(url)), err, fromPRForkURL)
	}
	out, err := exec.Command("git", "-C", ws, "rev-parse", "--abbrev-ref", "@{u}").Output()
	if err != nil || strings.TrimSpace(string(out)) != "fork/fix-1" {
		t.Fatalf("upstream = %q, %v; want fork/fix-1", strings.TrimSpace(string(out)), err)
	}
}

// Without gh the command still lands the PR head, named pr-<n>, with nothing
// wired for pushing back.
func TestRunWorkspaceFromPRWithoutGh(t *testing.T) {
	env := setupFromPREnv(t)
	stubPRView(t, func(host, repo string, number int) (forge.PR, error) {
		return forge.PR{}, forge.ErrGhMissing
	})

	captureStdout(t, func() {
		if err := runWorkspaceFromPR("widget#7", ""); err != nil {
			t.Fatalf("runWorkspaceFromPR: %v", err)
		}
	})

	ws := workspace.PathFor(fromPRRepoName, "pr-7")
	if got := revParseT(t, ws, "HEAD"); got != env.featureSHA {
		t.Fatalf("HEAD = %s, want the PR head %s", got, env.featureSHA)
	}
	// No upstream tracking: there is nowhere sensible to push refs/pull/*.
	if out, err := exec.Command("git", "-C", ws, "rev-parse", "--abbrev-ref", "@{u}").Output(); err == nil {
		t.Fatalf("upstream = %q, want none", strings.TrimSpace(string(out)))
	}
}

// --name overrides the workspace name and still lands the PR head.
func TestRunWorkspaceFromPRNameOverride(t *testing.T) {
	env := setupFromPREnv(t)
	stubPRView(t, func(host, repo string, number int) (forge.PR, error) {
		return forge.PR{Number: 7, HeadRefName: "feature-x", State: "OPEN"}, nil
	})

	captureStdout(t, func() {
		if err := runWorkspaceFromPR("widget#7", "my-review"); err != nil {
			t.Fatalf("runWorkspaceFromPR: %v", err)
		}
	})

	ws := workspace.PathFor(fromPRRepoName, "my-review")
	if got := revParseT(t, ws, "HEAD"); got != env.featureSHA {
		t.Fatalf("HEAD = %s, want the PR head %s", got, env.featureSHA)
	}
	if br, err := workspace.CurrentBranch(ws); err != nil || br != "my-review" {
		t.Fatalf("branch = %q, %v; want my-review", br, err)
	}
}

// A PR lookup failure that is not "gh unavailable" (no such PR, network down)
// must not create anything — a workspace silently off the wrong commit is the
// failure mode from-pr exists to prevent.
func TestRunWorkspaceFromPRLookupFailure(t *testing.T) {
	setupFromPREnv(t)
	stubPRView(t, func(host, repo string, number int) (forge.PR, error) {
		return forge.PR{}, errors.New("gh pr view failed: some transient network problem")
	})

	err := runWorkspaceFromPR("widget#999", "")
	if err == nil {
		t.Fatal("want an error for a failed PR lookup")
	}
	if infos, _ := workspace.ListAll(); len(infos) != 0 {
		t.Fatalf("no workspace should exist after a failed lookup, got %+v", infos)
	}
}

// A PR that GitHub says does not exist is a NotFound, not a network error —
// scripts keying on exit codes must not retry a typo'd number. The failed run
// must also consume the pending session intent its pre-exec hook recorded, so
// no stale intent survives to mis-link a future workspace.
func TestRunWorkspaceFromPRNotFound(t *testing.T) {
	setupFromPREnv(t)
	stubPRView(t, func(host, repo string, number int) (forge.PR, error) {
		return forge.PR{}, fmt.Errorf("%w: #999 in acme/widget", forge.ErrPRNotFound)
	})
	if err := workspace.WritePending(prPendingKey("widget", 999), workspace.SessionLink{
		Agent: "claude", SessionID: "sess-dead", CWD: "/x",
	}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}

	err := runWorkspaceFromPR("widget#999", "")
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.NotFound {
		t.Fatalf("err = %v, want errs.NotFound", err)
	}
	if p, _ := workspace.TakePending(prPendingKey("widget", 999)); p != nil {
		t.Fatalf("pending intent should be consumed by the failed run, got %+v", *p)
	}
}

// A repo that is not in the library fails with a pointer at `shed add`.
func TestRunWorkspaceFromPRUnknownRepo(t *testing.T) {
	tempHome(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	saveConfig(t, &config.Config{})
	stubPRView(t, func(host, repo string, number int) (forge.PR, error) {
		t.Error("prView should not be called for an unknown repo")
		return forge.PR{}, nil
	})

	err := runWorkspaceFromPR("https://github.com/octocat/Hello-World/pull/42", "")
	var coded *errs.Coded
	if !errors.As(err, &coded) || coded.Code != errs.NotFound {
		t.Fatalf("err = %v, want errs.NotFound", err)
	}
	if !strings.Contains(err.Error(), "shed add octocat/Hello-World") {
		t.Fatalf("err = %q, want a `shed add octocat/Hello-World` hint", err)
	}
}

// The session link recorded by the pre-exec hook under "pr-<n>" (the hook
// can't know the eventual workspace name) is finalized onto the workspace,
// whatever it ended up being named — this is what makes `shed resume` work
// for from-pr workspaces.
func TestRunWorkspaceFromPRLinksSession(t *testing.T) {
	setupFromPREnv(t)
	stubPRView(t, func(host, repo string, number int) (forge.PR, error) {
		return forge.PR{Number: 7, HeadRefName: "feature-x", State: "OPEN"}, nil
	})
	// What the __on-tool-call hook records for `shed workspace from-pr widget#7`
	// — the repo-scoped rendezvous key both sides derive from the command.
	if err := workspace.WritePending(prPendingKey("widget", 7), workspace.SessionLink{
		Agent: "claude", SessionID: "sess-42", CWD: "/launch/dir",
	}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}

	captureStdout(t, func() {
		if err := runWorkspaceFromPR("widget#7", ""); err != nil {
			t.Fatalf("runWorkspaceFromPR: %v", err)
		}
	})

	link, err := workspace.LoadLink(fromPRRepoName, "feature-x")
	if err != nil || link == nil {
		t.Fatalf("LoadLink: link=%v err=%v; want the pending pr-7 intent finalized", link, err)
	}
	if link.Agent != "claude" || link.SessionID != "sess-42" {
		t.Fatalf("link = %+v, want agent=claude id=sess-42", *link)
	}
}
