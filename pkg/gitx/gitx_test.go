package gitx

import (
	"io"
	"strings"
	"testing"
)

// A failing git command's stderr must survive into RunProgress's returned
// output even while being streamed to a progress writer. This is a regression
// test for a shared-buffer race: stdout and stderr are copied on separate
// goroutines, and with a plain bytes.Buffer shared between them, io.Copy's
// ReadFrom fast path on the (empty) stdout side truncated away the captured
// stderr at EOF — so a failed clone reported "(output: )" with git's actual
// message ("Repository not found", auth errors) lost, and sync's gone-upstream
// classification, which reads that text, never fired.
func TestRunProgressCapturesStderrOnFailure(t *testing.T) {
	if err := RequireGit(); err != nil {
		t.Skip("git not installed")
	}
	// `git -C <empty dir> status` fails writing "not a git repository" to
	// stderr and nothing to stdout — the exact shape that triggered the race.
	dir := t.TempDir()
	out, err := RunProgress(io.Discard, nil, "-C", dir, "status")
	if err == nil {
		t.Fatal("git status outside a repository should fail")
	}
	if !strings.Contains(strings.ToLower(string(out)), "not a git repository") {
		t.Fatalf("returned output should carry git's stderr, got %q", out)
	}
}
