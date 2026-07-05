package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewHannigan/shed/pkg/agents"
	"github.com/AndrewHannigan/shed/pkg/errs"
	"github.com/AndrewHannigan/shed/pkg/paths"
	"github.com/AndrewHannigan/shed/pkg/workspace"
)

// runUninstall reverses agent integration for the selected agents and,
// when purge is set, deletes shed's config and data directories. It is
// the implementation behind 'shed init --uninstall'.
func runUninstall(flag string, purge bool) error {
	list, err := agents.SelectByFlag(flag)
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "no agents selected")
		// Still honor --purge even when there's no integration to reverse.
		if purge {
			return runPurge()
		}
		return nil
	}
	state, err := agents.LoadState()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	for _, a := range list {
		prev := state.Agents[a.Key()]
		fmt.Printf("%s:\n", a.Name())
		if err := a.Uninstall(prev); err != nil {
			fmt.Printf("  error: %v\n", err)
			continue
		}
		delete(state.Agents, a.Key())
		if len(prev.AddedFiles) > 0 {
			fmt.Printf("  removed %d directories, %d hooks, %d plugin file(s)\n",
				len(prev.AddedPaths), len(prev.AddedHooks), len(prev.AddedFiles))
		} else {
			fmt.Printf("  removed %d directories, %d hooks\n",
				len(prev.AddedPaths), len(prev.AddedHooks))
		}
	}
	if err := agents.SaveState(state); err != nil {
		return errs.Wrap(errs.Config, err)
	}
	if purge {
		return runPurge()
	}
	return nil
}

// runPurge deletes shed's config and data directories. It first
// scans workspaces for uncommitted or unpushed work and, if any is
// found, prints them and asks for confirmation before deleting.
func runPurge() error {
	dirty, err := dirtyWorkspaces()
	if err != nil {
		return errs.Wrap(errs.Config, err)
	}
	if len(dirty) > 0 {
		fmt.Fprintf(os.Stderr, "\nWARNING: %d workspace(s) have uncommitted or unpushed work:\n", len(dirty))
		for _, w := range dirty {
			fmt.Fprintf(os.Stderr, "  %s  (%s)\n", paths.Display(w.Path), describeDirty(w))
		}
		if !confirmPurge() {
			fmt.Fprintln(os.Stderr, "aborted; nothing deleted")
			return nil
		}
	}

	for _, dir := range []string{paths.DataDir(), paths.ConfigDir()} {
		if err := removeAllForce(dir); err != nil {
			return errs.Wrap(errs.Config, fmt.Errorf("remove %s: %w", dir, err))
		}
		fmt.Printf("removed %s\n", paths.Display(dir))
	}
	return nil
}

// removeAllForce deletes dir like os.RemoveAll, but first restores the
// owner write bit on every directory in the tree. sync leaves stored repos
// chmod a-w (see catalog.LockTree), and os.RemoveAll cannot unlink entries
// inside a directory that lacks write permission, so a plain RemoveAll
// fails partway through with EACCES. Only directories need fixing:
// removing an entry depends on the parent dir's mode, not the entry's own.
func removeAllForce(dir string) error {
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // missing/unreadable: let RemoveAll report it
		}
		if info.IsDir() && info.Mode().Perm()&0200 == 0 {
			os.Chmod(p, info.Mode().Perm()|0200)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// dirtyWorkspaces returns every workspace with uncommitted changes or
// unpushed commits.
func dirtyWorkspaces() ([]workspace.Info, error) {
	all, err := workspace.ListAll()
	if err != nil {
		return nil, err
	}
	var dirty []workspace.Info
	for _, w := range all {
		if w.Dirty || w.Unpushed > 0 {
			dirty = append(dirty, w)
		}
	}
	return dirty, nil
}

func describeDirty(w workspace.Info) string {
	var parts []string
	if w.Dirty {
		parts = append(parts, "uncommitted changes")
	}
	if w.Unpushed > 0 {
		parts = append(parts, fmt.Sprintf("%d unpushed commit(s)", w.Unpushed))
	}
	return strings.Join(parts, ", ")
}

func confirmPurge() bool {
	if !stdinIsTTY() {
		// Non-interactive: refuse rather than destroy dirty work silently.
		fmt.Fprintln(os.Stderr, "refusing to purge dirty workspaces without an interactive confirmation")
		return false
	}
	fmt.Fprint(os.Stderr, "\nDelete all shed data and config anyway? [y/N] ")
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}
