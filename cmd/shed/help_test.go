package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected and returns what was written.
// Needed because the help command prints via fmt.Print (os.Stdout) while the
// root help func writes to cmd.OutOrStdout() — capturing the real stdout covers
// both.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

func runShed(args ...string) {
	root := newRootCmd()
	root.SetArgs(args)
	_ = root.Execute()
}

// A bare `shed`, `shed help`, and `shed --help` must all print the exact same
// curated overview. Historically the flag form fell back to Cobra's
// auto-generated usage, which confusingly differed from `shed help`.
func TestHelpPathsConsistent(t *testing.T) {
	bare := captureStdout(t, func() { runShed() })
	helpCmd := captureStdout(t, func() { runShed("help") })
	helpFlag := captureStdout(t, func() { runShed("--help") })

	for name, got := range map[string]string{
		"bare shed":   bare,
		"shed help":   helpCmd,
		"shed --help": helpFlag,
	} {
		if !strings.HasPrefix(strings.TrimSpace(got), overviewTopline) {
			t.Errorf("%s should start with the curated topline %q, got:\n%s", name, overviewTopline, got)
		}
	}
	if bare != helpCmd || helpCmd != helpFlag {
		t.Errorf("help paths diverge:\n--- bare ---\n%s\n--- help ---\n%s\n--- --help ---\n%s", bare, helpCmd, helpFlag)
	}
}

// `shed help <command>` should resolve a command name to the topic that
// documents it (e.g. add → catalog), not error out.
func TestHelpCommandAliases(t *testing.T) {
	for _, cmd := range []string{"add", "rm", "ls", "repo", "new", "path"} {
		out := captureStdout(t, func() { runShed("help", cmd) })
		if strings.Contains(out, "unknown topic") || strings.TrimSpace(out) == "" {
			t.Errorf("`shed help %s` should resolve to a topic, got:\n%s", cmd, out)
		}
	}
}
