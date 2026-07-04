package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/mirror"
	"github.com/AndrewHannigan/shed/pkg/paths"
)

const bgSyncWorkerEnv = "SHED_BG_SYNC_WORKER"

// storeEmptyHint nudges the user to run the first sync when repos are
// tracked but nothing has ever been fetched. It is printed to stdout for the
// plain-stdout agents.
const storeEmptyHint = "shed: no repos synced yet. Run `shed sync` to fetch your tracked repos."

func newBgSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "__bg-sync",
		Short:  "(internal) Background sync invoked by session-start hooks",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv(bgSyncWorkerEnv) == "1" {
				return bgSyncWorker()
			}
			if bgSyncStart() {
				fmt.Println(storeEmptyHint)
			}
			return nil
		},
	}
	return cmd
}

// bgSyncStart runs the session-start bg-sync action in the hook's process: it
// does nothing if no repos are tracked, returns true on first run (nothing ever
// synced) so the caller can surface storeEmptyHint, and otherwise spawns the
// detached worker. It never fails — the hook must not break the agent.
func bgSyncStart() (showEmptyHint bool) {
	c, err := config.Load()
	if err != nil || (len(c.Repos) == 0 && len(c.Owners) == 0) {
		return false
	}
	if !everSynced(c) {
		return true
	}
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	cmd := exec.Command(self, "__bg-sync")
	cmd.Env = append(os.Environ(), bgSyncWorkerEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if logFile, lerr := openBgLog(); lerr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	cmd.Stdin = nil
	_ = cmd.Start()
	return false
}

// bgSyncWorker runs as the detached child. Acquires the global lock
// non-blocking; if held, exits silently. Otherwise runs the standard
// sync with --if-older-than from config.
func bgSyncWorker() error {
	if err := os.MkdirAll(paths.DataDir(), 0755); err != nil {
		return nil
	}
	lock := flock.New(paths.BgSyncLockFile())
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return nil
	}
	defer lock.Unlock()
	return runSync(nil, 4, configBgInterval(), false)
}

func everSynced(c *config.Config) bool {
	for _, r := range c.Repos {
		key, err := r.MirrorKey()
		if err != nil {
			continue
		}
		if meta, _ := mirror.LoadMeta(key); meta != nil {
			return true
		}
	}
	return false
}

// configBgInterval returns the staleness gate passed to `sync
// --if-older-than`. Default is 0 (no gate): the store refreshes on every
// session start. Set bg_sync_interval (e.g. "1h") to skip repos synced
// within that window instead.
func configBgInterval() time.Duration {
	c, err := config.Load()
	if err != nil {
		return 0
	}
	if c.Settings.BgSyncInterval == "" {
		return 0
	}
	d, err := time.ParseDuration(c.Settings.BgSyncInterval)
	if err != nil {
		return 0
	}
	return d
}

// bgLogMaxBytes is the size at which openBgLog rotates the bg-sync log so it
// can't grow unbounded across sessions.
const bgLogMaxBytes = 5 << 20 // 5 MB

func openBgLog() (*os.File, error) {
	path := paths.BgSyncLogFile()
	// Single-backup rotation: when the log reaches the cap, move it aside to
	// <log>.1 (replacing any previous backup) and start a fresh file.
	if fi, err := os.Stat(path); err == nil && fi.Size() >= bgLogMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}
