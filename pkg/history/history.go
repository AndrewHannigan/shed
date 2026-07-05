// Package history records recent shed command invocations to a small
// on-disk log, so the human (via `shed history`) and the agent (via the
// session-context hook) can see what was recently done. The log is a JSON-Lines
// file, appended to on each tracked command and periodically truncated so it
// never grows without bound.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewHannigan/shed/pkg/paths"
)

const (
	// maxEvents is the number of events the log is truncated back to.
	maxEvents = 200
	// trimInterval is the minimum time between truncation checks, so the log
	// is read+rewritten at most once per interval rather than on every command.
	trimInterval = 5 * time.Minute
)

// Event is one recorded command invocation. Args is the raw argument vector
// (os.Args[1:]) as the user typed it, so the rendered line is faithful.
type Event struct {
	Time time.Time `json:"t"`
	Args []string  `json:"args"`
}

// Record appends one command invocation to the history log, then opportunistically
// trims it. Best-effort: it is a no-op (returns nil) when shed's data dir
// does not exist, and it never creates that dir — so it neither materializes the
// data dir on its own nor resurrects it after `init --uninstall --purge`.
func Record(args []string) error {
	dir := paths.DataDir()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	line, err := json.Marshal(Event{Time: time.Now().UTC(), Args: args})
	if err != nil {
		return err
	}
	// The log lives under .internal/, which may not exist yet on an older or
	// freshly-initialized shed; creating it does not resurrect the data dir
	// (the stat above already guards that).
	if err := os.MkdirAll(filepath.Dir(paths.HistoryFile()), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(paths.HistoryFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	maybeTrim()
	return nil
}

// Recent returns up to n of the most recent events, oldest first. A missing
// history file yields an empty slice (not an error); malformed lines are skipped.
// A negative n returns everything.
func Recent(n int) ([]Event, error) {
	events, err := readAll()
	if err != nil {
		return nil, err
	}
	if n >= 0 && len(events) > n {
		events = events[len(events)-n:]
	}
	return events, nil
}

// readAll parses the whole history file into events, oldest first. Torn or
// corrupt lines (e.g. a partially written final line) are skipped rather than
// failing the read.
func readAll() ([]Event, error) {
	f, err := os.Open(paths.HistoryFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// maybeTrim truncates the history file to the most recent maxEvents, but at most
// once per trimInterval. A marker file records the last trim-check time; the
// whole-file read+rewrite runs only once the marker is that old (or absent). The
// marker is rewritten on every check, so checks — and thus rewrites — happen no
// more than once per interval regardless of command volume. Best-effort: any
// error leaves the log untouched.
func maybeTrim() {
	marker := paths.HistoryTrimMarkerFile()
	if data, err := os.ReadFile(marker); err == nil {
		if last, perr := time.Parse(time.RFC3339, strings.TrimSpace(string(data))); perr == nil && time.Since(last) < trimInterval {
			return
		}
	}
	// Gate is open: record the check time up front so rapid/concurrent callers
	// don't repeat the work, then trim if the log is over the cap.
	_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)), 0644)

	events, err := readAll()
	if err != nil || len(events) <= maxEvents {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, ev := range events[len(events)-maxEvents:] {
		if err := enc.Encode(ev); err != nil {
			return
		}
	}
	tmp := paths.HistoryFile() + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, paths.HistoryFile())
}
