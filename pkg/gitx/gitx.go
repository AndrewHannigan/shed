// Package gitx holds the small shared plumbing for shelling out to git:
// running commands with captured output, streaming git's live progress meter,
// and the presence/reachability probes every tier (mirror, catalog,
// workspace) depends on.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// ErrGitMissing is returned by RequireGit when the git binary is not on PATH.
// git is shed's one hard dependency — every mirror, catalog, and workspace
// operation shells out to it (clone, fetch, checkout) — so it is the one thing
// we cannot degrade around the way we do for gh (see pkg/forge).
var ErrGitMissing = errors.New("git not found on PATH — shed requires git (install from https://git-scm.com/downloads)")

// RequireGit reports whether the git binary is available on PATH, returning
// ErrGitMissing if not. Commands that shell out to git call this first so the
// user gets one clear, actionable message instead of a cryptic per-invocation
// "exec: \"git\": executable file not found in $PATH".
func RequireGit() error {
	if _, err := exec.LookPath("git"); err != nil {
		return ErrGitMissing
	}
	return nil
}

// Run executes `git -C dir <args>` and returns an error wrapping git's
// combined output on failure. dir == "" runs without -C.
func Run(dir string, args ...string) error {
	_, err := RunOut(dir, args...)
	return err
}

// RunOut is Run returning the combined output too (for callers that parse it).
func RunOut(dir string, args ...string) ([]byte, error) {
	return RunEnvOut(dir, nil, args...)
}

// RunEnv is Run with extra environment variables appended to the inherited
// environment (e.g. GIT_LFS_SKIP_SMUDGE=1).
func RunEnv(dir string, extraEnv []string, args ...string) error {
	_, err := RunEnvOut(dir, extraEnv, args...)
	return err
}

// RunEnvOut executes `git -C dir <args>` with extra environment variables and
// returns the combined output; on failure the error wraps that output so the
// caller's message carries git's own words.
func RunEnvOut(dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", withDir(dir, args)...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w (output: %s)", firstArg(args), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Output executes `git -C dir <args>` and returns trimmed stdout only.
func Output(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", withDir(dir, args)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", firstArg(args), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RunProgress runs `git <args>` (no implied -C), returning the combined output
// for error reporting. When progress is non-nil, git's stderr is additionally
// streamed there live so the user sees the progress meter as it advances.
// Callers that want a meter must also add `--progress` to args: stderr here is
// a pipe (the MultiWriter, not the terminal), so git would otherwise suppress
// the meter as it does for any non-TTY. With progress nil this is exactly
// CombinedOutput() behavior.
func RunProgress(progress io.Writer, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if progress == nil {
		return cmd.CombinedOutput()
	}
	// Tee stderr to the caller (the terminal) and a buffer: the user watches it
	// scroll by, and a copy is still on hand for the error message if it fails.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.MultiWriter(progress, &buf)
	err := cmd.Run()
	return buf.Bytes(), err
}

// RefExists reports whether the exact ref (e.g. "refs/remotes/origin/main")
// exists in the repository at dir.
func RefExists(dir, ref string) (bool, error) {
	cmd := exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", ref)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// RevParse resolves rev to a full object ID in the repository at dir,
// returning ok=false when the rev does not resolve.
func RevParse(dir, rev string) (sha string, ok bool) {
	out, err := Output(dir, "rev-parse", "--verify", "--quiet", rev)
	if err != nil || out == "" {
		return "", false
	}
	return out, true
}

// Reachable probes whether git can authenticate to and read from url without
// any interactive prompt. It runs `git ls-remote` with terminal and SSH
// prompts disabled, so a missing or wrong credential fails fast instead of
// blocking on stdin (which would hang an `add` or a session-start hook). A nil
// return means the remote is reachable with the credentials currently
// configured for whatever transport url names. A non-nil error wraps git's
// output so callers can classify it (auth vs. network vs. not-found).
func Reachable(url string) error {
	cmd := exec.Command("git", "ls-remote", "--heads", "--", url)
	// GIT_TERMINAL_PROMPT=0 stops HTTPS from prompting for username/password.
	// BatchMode=yes does the same for SSH (no passphrase/password prompt);
	// accept-new avoids hanging on an unknown host key the first time.
	// Respect a user's own GIT_SSH_COMMAND if they've set one.
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes -oStrictHostKeyChecking=accept-new")
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git ls-remote: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func withDir(dir string, args []string) []string {
	if dir == "" {
		return args
	}
	return append([]string{"-C", dir}, args...)
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
