package mirror

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/AndrewHannigan/shed/pkg/paths"
)

// CatalogStatus is one catalog repo's sync record, kept on its mirror's meta
// (keyed by repo name) — deliberately NOT in git's worktrees/<id>/ admin dir,
// which `git worktree prune` deletes exactly when a broken repo's record
// matters. LastSyncAt records the last *successful* update of that catalog;
// LastError/LastErrorAt record the most recent failed attempt (cleared on the
// next success).
type CatalogStatus struct {
	LastSyncAt  time.Time `json:"last_sync_at"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
}

// Meta is the JSON sidecar at <mirror>/.git/shed.meta. The mirror-level
// fields describe the fetch (the shared network step); Catalogs holds one
// record per catalog repo of this mirror.
type Meta struct {
	LastSyncAt  time.Time                `json:"last_sync_at"`
	LastError   string                   `json:"last_error,omitempty"`
	LastErrorAt time.Time                `json:"last_error_at,omitempty"`
	Catalogs    map[string]CatalogStatus `json:"catalogs,omitempty"`
}

// LoadMeta reads the mirror's meta sidecar. Returns (nil, nil) if absent (the
// mirror hasn't been synced yet, or doesn't exist).
func LoadMeta(key string) (*Meta, error) {
	data, err := os.ReadFile(paths.MirrorMetaFile(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// SaveMeta writes the mirror's meta sidecar. Callers hold the mirror lock.
func SaveMeta(key string, m *Meta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p := paths.MirrorMetaFile(key)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0644)
}

// mutateMeta loads, edits, and saves the mirror's meta in one step (callers
// hold the mirror lock). A missing meta starts empty.
func mutateMeta(key string, fn func(*Meta)) error {
	m, err := LoadMeta(key)
	if err != nil || m == nil {
		m = &Meta{}
	}
	fn(m)
	return SaveMeta(key, m)
}

// RecordFetchOK stamps a successful mirror fetch, clearing any prior
// mirror-level error.
func RecordFetchOK(key string, at time.Time) error {
	return mutateMeta(key, func(m *Meta) {
		m.LastSyncAt = at
		m.LastError = ""
		m.LastErrorAt = time.Time{}
	})
}

// RecordFetchError persists a failed mirror fetch (best effort — the mirror
// may not exist yet, in which case there is nowhere to write and callers use
// the standalone first-sync records instead).
func RecordFetchError(key, errText string) error {
	if !Exists(key) {
		return nil
	}
	return mutateMeta(key, func(m *Meta) {
		m.LastError = errText
		m.LastErrorAt = time.Now().UTC()
	})
}

// RecordCatalogOK stamps a successful catalog update, clearing any prior
// error for that repo.
func RecordCatalogOK(key, name string, at time.Time) error {
	return mutateMeta(key, func(m *Meta) {
		if m.Catalogs == nil {
			m.Catalogs = make(map[string]CatalogStatus)
		}
		cs := m.Catalogs[name]
		cs.LastSyncAt = at
		cs.LastError = ""
		cs.LastErrorAt = time.Time{}
		m.Catalogs[name] = cs
	})
}

// RecordCatalogError persists a failed catalog update, keeping the prior
// LastSyncAt so status can still show the last good sync.
func RecordCatalogError(key, name, errText string) error {
	if !Exists(key) {
		return nil
	}
	return mutateMeta(key, func(m *Meta) {
		if m.Catalogs == nil {
			m.Catalogs = make(map[string]CatalogStatus)
		}
		cs := m.Catalogs[name]
		cs.LastError = errText
		cs.LastErrorAt = time.Now().UTC()
		m.Catalogs[name] = cs
	})
}

// DropCatalog removes a repo's record from the mirror meta (called by rm so a
// removed repo's stale record can't resurface in status).
func DropCatalog(key, name string) error {
	if !Exists(key) {
		return nil
	}
	return mutateMeta(key, func(m *Meta) {
		delete(m.Catalogs, name)
	})
}

// StatusFor returns the effective sync status for one catalog repo: its own
// record merged with the mirror-level fetch state (a fetch failure stales
// every catalog of that mirror, so the more recent of the two errors wins).
// Returns nil when the mirror has no meta at all.
func StatusFor(key, name string) *CatalogStatus {
	m, err := LoadMeta(key)
	if err != nil || m == nil {
		return nil
	}
	out, known := m.Catalogs[name]
	if m.LastError != "" && m.LastErrorAt.After(out.LastErrorAt) {
		out.LastError = m.LastError
		out.LastErrorAt = m.LastErrorAt
	}
	if !known && out.LastError == "" {
		// The mirror has synced but this repo never has (it was added later,
		// sharing an existing mirror): report "no record", not a clean sync.
		return nil
	}
	return &out
}

// RecordFirstSyncError persists errText for a repo that failed before its
// mirror (and meta sidecar) ever existed — i.e. a failed first clone.
// Without it the failure would vanish: LoadMeta has nothing to read, so
// `shed status` would report the repo healthy and the session-context banner
// would stay silent. The record lives in a standalone file outside ReposDir
// so it can never be mistaken for a materialized catalog repo.
func RecordFirstSyncError(name, errText string) error {
	m := &CatalogStatus{LastError: errText, LastErrorAt: time.Now().UTC()}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p := paths.SyncErrorFile(name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0644)
}

// LoadFirstSyncError reads a standalone first-sync failure record written by
// RecordFirstSyncError, or (nil, nil) when none exists.
func LoadFirstSyncError(name string) (*CatalogStatus, error) {
	data, err := os.ReadFile(paths.SyncErrorFile(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var m CatalogStatus
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ClearFirstSyncError removes any standalone first-sync failure record for
// name (best effort), pruning now-empty parent dirs. Called after a
// successful sync and on rm so a stale record can't outlive the condition it
// described.
func ClearFirstSyncError(name string) {
	p := paths.SyncErrorFile(name)
	if err := os.Remove(p); err != nil {
		return
	}
	paths.PruneEmptyDirs(filepath.Dir(p), paths.SyncErrorDir())
}
