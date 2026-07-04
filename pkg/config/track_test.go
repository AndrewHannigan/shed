package config

import (
	"strings"
	"testing"
)

// Names derive from URL and track: a tracked ref gains an "@<track>" suffix,
// with slashes in the track sanitized to "-" so the name stays one leaf dir.
func TestResolvedNameWithTrack(t *testing.T) {
	cases := []struct {
		repo Repo
		want string
	}{
		{Repo{URL: "https://github.com/apache/airflow"}, "github.com/apache/airflow"},
		{Repo{URL: "https://github.com/apache/airflow", Track: "v2-7-stable"}, "github.com/apache/airflow@v2-7-stable"},
		{Repo{URL: "https://github.com/apache/airflow", Track: "release/2.8"}, "github.com/apache/airflow@release-2.8"},
		{Repo{URL: "https://github.com/apache/airflow", Track: "tags/2.7.3"}, "github.com/apache/airflow@2.7.3"},
		{Repo{URL: "https://github.com/apache/airflow", Track: "x", Name: "override"}, "override"},
	}
	for _, tc := range cases {
		got, err := tc.repo.ResolvedName()
		if err != nil || got != tc.want {
			t.Errorf("ResolvedName(%+v) = %q, %v; want %q", tc.repo, got, err, tc.want)
		}
	}
}

// One repo per (upstream, track) is an invariant, enforced even under
// explicit name overrides, and keyed by the mirror identity so the same ref
// over two transports still counts as a duplicate.
func TestValidateRejectsDuplicateURLTrack(t *testing.T) {
	c := &Config{Repos: []Repo{
		{URL: "https://github.com/apache/airflow", Track: "v2", Name: "one"},
		{URL: "git@github.com:apache/airflow.git", Track: "v2", Name: "two"},
	}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "same upstream") {
		t.Fatalf("want a duplicate (url, track) error, got %v", err)
	}
}

// Two tracks that sanitize to the same directory collide by name and are
// rejected — the sanitize mapping is lossy, so config refuses the ambiguity.
func TestValidateRejectsSanitizeCollision(t *testing.T) {
	c := &Config{Repos: []Repo{
		{URL: "https://github.com/apache/airflow", Track: "release/2.8"},
		{URL: "https://github.com/apache/airflow", Track: "release-2.8"},
	}}
	if err := c.Validate(); err == nil {
		t.Fatal("tracks sanitizing to one name must be rejected")
	}
}

// An unsafe track (traversal, option injection) never reaches git.
func TestValidateRejectsUnsafeTrack(t *testing.T) {
	for _, track := range []string{"-evil", "../../etc", "heads/-x", "a//b"} {
		c := &Config{Repos: []Repo{{URL: "https://github.com/a/b", Track: track}}}
		if err := c.Validate(); err == nil {
			t.Errorf("track %q should be rejected", track)
		}
	}
	// Sanity: normal tracks pass.
	for _, track := range []string{"main", "v2-7-stable", "release/2.8", "tags/2.7.3", "heads/2.7.3"} {
		c := &Config{Repos: []Repo{{URL: "https://github.com/a/b", Track: track}}}
		if err := c.Validate(); err != nil {
			t.Errorf("track %q should be accepted, got %v", track, err)
		}
	}
}

// Entries sharing a mirror but disagreeing on transport get a warning (the
// fetch uses the first entry's URL), never an error.
func TestWarningsSharedMirrorTransport(t *testing.T) {
	c := &Config{Repos: []Repo{
		{URL: "https://github.com/apache/airflow"},
		{URL: "git@github.com:apache/airflow.git", Track: "v2"},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("transport disagreement must not fail validation: %v", err)
	}
	w := c.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "transport") {
		t.Fatalf("want one transport warning, got %v", w)
	}

	same := &Config{Repos: []Repo{
		{URL: "https://github.com/apache/airflow"},
		{URL: "https://github.com/apache/airflow", Track: "v2"},
	}}
	if w := same.Warnings(); len(w) != 0 {
		t.Fatalf("same transport should not warn, got %v", w)
	}
}
