package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveAllForce verifies removeAllForce deletes a tree containing
// read-only directories (mode a-w) — the state sync leaves stored repos in
// (see catalog.LockTree), and what made a plain os.RemoveAll-based --purge
// fail partway through with EACCES.
func TestRemoveAllForce(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "repo", "cmd")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "sync.go"), []byte("package main\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Lock the tree read-only from the leaves up, as catalog.LockTree does.
	// A read-only parent directory is what blocks unlinking its children.
	for _, d := range []string{sub, filepath.Join(root, "repo")} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}

	if err := removeAllForce(root); err != nil {
		t.Fatalf("removeAllForce: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root still present after removeAllForce: %v", err)
	}
}

// TestRemoveAllForceMissing confirms removing an absent path is a no-op,
// so purging when a directory was never created (or already gone) succeeds.
func TestRemoveAllForceMissing(t *testing.T) {
	if err := removeAllForce(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatalf("removeAllForce on missing path: %v", err)
	}
}
