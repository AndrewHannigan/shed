package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// git runs a git command in dir with a pinned identity, failing on error.
// extraEnv adds per-command variables (e.g. GIT_COMMITTER_DATE) — the reflog
// entry's own timestamp, which lastActivity reads, follows GIT_COMMITTER_DATE,
// so a date is only forced on the commands meant to be backdated, never on the
// clone whose reflog time must reflect real "creation now".
func git(t *testing.T, dir string, extraEnv []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	cmd.Env = append(cmd.Env, extraEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestLastActivity is the regression for the ACTIVE column: a workspace cloned
// from a repo whose newest commit is ancient must report its own (recent)
// creation time, not the commit's date — and must advance once work is
// committed.
func TestLastActivity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	ancient := []string{
		"GIT_AUTHOR_DATE=2010-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2010-01-01T00:00:00Z",
	}

	// Upstream repo whose only commit is backdated to 2010.
	upstream := filepath.Join(root, "upstream")
	git(t, root, nil, "init", "-q", upstream)
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, upstream, nil, "add", "a.txt")
	git(t, upstream, ancient, "commit", "-q", "-m", "ancient")

	// Clone it "now" (no date override) — this is the workspace creation.
	ws := filepath.Join(root, "ws")
	git(t, root, nil, "clone", "-q", upstream, ws)

	age := lastActivity(ws)
	if age.IsZero() {
		t.Fatal("lastActivity returned zero time for a fresh clone")
	}
	if time.Since(age) > time.Hour {
		t.Fatalf("fresh clone age = %v (%v ago); want recent creation time, not the 2010 commit date",
			age, time.Since(age))
	}

	// Committing new work advances the age to the commit's action time.
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, nil, "add", "a.txt")
	git(t, ws, nil, "commit", "-q", "-m", "new work")

	after := lastActivity(ws)
	if time.Since(after) > time.Hour {
		t.Fatalf("post-commit age = %v (%v ago); want recent commit time", after, time.Since(after))
	}
	if after.Before(age) {
		t.Fatalf("age went backwards after a commit: before=%v after=%v", age, after)
	}
}

func TestParseReflogUnix(t *testing.T) {
	cases := []struct {
		name     string
		selector string
		want     int64 // unix seconds; -1 means expect the zero time
	}{
		{"clone entry", "HEAD@{1782599155}\n", 1782599155},
		{"trailing newline trimmed inside braces", "HEAD@{ 1782599155 }", 1782599155},
		{"non-unix selector falls back to zero", "HEAD@{0}x", 0},
		{"missing braces", "HEAD", -1},
		{"empty", "", -1},
		{"non-numeric", "HEAD@{abc}", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseReflogUnix(c.selector)
			if c.want < 0 {
				if !got.IsZero() {
					t.Fatalf("parseReflogUnix(%q) = %v, want zero time", c.selector, got)
				}
				return
			}
			if got.Unix() != c.want {
				t.Fatalf("parseReflogUnix(%q).Unix() = %d, want %d", c.selector, got.Unix(), c.want)
			}
		})
	}
}

func TestCloneArgs(t *testing.T) {
	t.Run("no git config", func(t *testing.T) {
		got := cloneArgs("/repos/x", "main", "/dest", nil)
		want := []string{"clone", "--branch", "main", "--", "/repos/x", "/dest"}
		assertArgs(t, got, want)
	})

	t.Run("no branch clones the catalog's HEAD", func(t *testing.T) {
		got := cloneArgs("/repos/x", "", "/dest", nil)
		want := []string{"clone", "--", "/repos/x", "/dest"}
		assertArgs(t, got, want)
	})

	t.Run("git config seeded as sorted --config flags before --", func(t *testing.T) {
		got := cloneArgs("/repos/x", "main", "/dest", map[string]string{
			"user.email":     "me@work.com",
			"core.hooksPath": ".githooks",
		})
		want := []string{
			"clone", "--branch", "main",
			"--config", "core.hooksPath=.githooks", // sorted: core before user
			"--config", "user.email=me@work.com",
			"--", "/repos/x", "/dest",
		}
		assertArgs(t, got, want)
	})
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("cloneArgs mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestLandedInDefault covers the three states prune reports differently: a
// branch whose own commits landed (a real merge), a branch that never committed
// anything (tip still on the default branch — must NOT be called "merged"), and
// a branch with commits not yet in the default branch (not landed at all).
func TestLandedInDefault(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()

	// Upstream with default branch "main". feat adds its own commit and is then
	// merged back with a merge commit, so its tip is contained in main but off
	// main's first-parent line.
	upstream := filepath.Join(root, "upstream")
	git(t, root, nil, "init", "-q", "-b", "main", upstream)
	writeFile(t, upstream, "a.txt", "1")
	git(t, upstream, nil, "add", "a.txt")
	git(t, upstream, nil, "commit", "-q", "-m", "first")
	git(t, upstream, nil, "checkout", "-q", "-b", "feat")
	writeFile(t, upstream, "b.txt", "2")
	git(t, upstream, nil, "add", "b.txt")
	git(t, upstream, nil, "commit", "-q", "-m", "feat work")
	git(t, upstream, nil, "checkout", "-q", "main")
	git(t, upstream, nil, "merge", "-q", "--no-ff", "-m", "merge feat", "feat")
	// Advance main once more so a never-committed branch sits behind its tip.
	writeFile(t, upstream, "c.txt", "3")
	git(t, upstream, nil, "add", "c.txt")
	git(t, upstream, nil, "commit", "-q", "-m", "later")

	ws := filepath.Join(root, "ws")
	git(t, root, nil, "clone", "-q", upstream, ws)

	tests := []struct {
		name       string
		setup      func()
		wantLanded bool
		wantOwn    bool
	}{
		{
			name: "merged with own commits",
			setup: func() {
				git(t, ws, nil, "checkout", "-q", "-B", "feat", "origin/feat")
			},
			wantLanded: true,
			wantOwn:    true,
		},
		{
			name: "never committed, tip still on main",
			setup: func() {
				// A fresh workspace branch at main's tip with no commits of its own.
				git(t, ws, nil, "checkout", "-q", "main")
				git(t, ws, nil, "checkout", "-q", "-B", "scratch")
			},
			wantLanded: true,
			wantOwn:    false,
		},
		{
			name: "not landed: has unmerged commit",
			setup: func() {
				git(t, ws, nil, "checkout", "-q", "main")
				git(t, ws, nil, "checkout", "-q", "-B", "ahead")
				writeFile(t, ws, "d.txt", "4")
				git(t, ws, nil, "add", "d.txt")
				git(t, ws, nil, "commit", "-q", "-m", "local only")
			},
			wantLanded: false,
			wantOwn:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			branch, err := exec.Command("git", "-C", ws, "rev-parse", "--abbrev-ref", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			landed, own, def, err := LandedInDefault(ws, strings.TrimSpace(string(branch)))
			if err != nil {
				t.Fatalf("LandedInDefault: %v", err)
			}
			if landed != tt.wantLanded || own != tt.wantOwn {
				t.Errorf("LandedInDefault = (landed=%v, own=%v), want (landed=%v, own=%v)",
					landed, own, tt.wantLanded, tt.wantOwn)
			}
			if landed && def != "main" {
				t.Errorf("default branch = %q, want %q", def, "main")
			}
		})
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
