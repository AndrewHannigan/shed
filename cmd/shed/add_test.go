package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/config"
	"github.com/AndrewHannigan/shed/pkg/errs"
)

// A just-added owner that manages at least one repo is working as intended, so
// no advisory is emitted.
func TestOwnerEmptyHintWithRepos(t *testing.T) {
	c := &config.Config{
		Owners: []config.Owner{{URL: "https://github.com/acme"}},
		Repos: []config.Repo{
			{URL: "https://github.com/acme/widget", Source: "github.com/acme"},
		},
	}
	if hint := ownerEmptyHint(c, "github.com/acme"); hint != "" {
		t.Fatalf("owner with a repo should produce no hint, got %q", hint)
	}
}

// An owner that exists (add validates that) but resolved to zero repos is the
// benign "empty owner" case; the hint names the owner and points at `shed rm`
// so an unexpected empty entry is easy to undo.
func TestOwnerEmptyHintNoRepos(t *testing.T) {
	c := &config.Config{
		Owners: []config.Owner{{URL: "https://github.com/emptyorg"}},
	}
	hint := ownerEmptyHint(c, "github.com/emptyorg")
	if hint == "" {
		t.Fatal("owner with no repos should produce a hint")
	}
	if !strings.Contains(hint, "github.com/emptyorg") {
		t.Fatalf("hint should name the owner, got %q", hint)
	}
	if !strings.Contains(hint, "shed rm github.com/emptyorg") {
		t.Fatalf("hint should suggest `shed rm <owner>`, got %q", hint)
	}
}

// preflightProbe builds a probe stub for preflightURL tests: it returns the
// error mapped for each URL (nil for a URL not in the map, i.e. reachable)
// and records the URLs probed, in order.
func preflightProbe(fail map[string]error, probed *[]string) func(string) error {
	return func(url string) error {
		*probed = append(*probed, url)
		return fail[url]
	}
}

func TestPreflightURL(t *testing.T) {
	httpsURL := "https://github.com/psf/requests"
	sshAlt := "git@github.com:psf/requests.git"
	notFound := errors.New(`git ls-remote: exit status 128 (output: remote: Repository not found.)`)
	authErr := errors.New(`git ls-remote: exit status 128 (output: fatal: could not read Username for 'https://github.com': terminal prompts disabled)`)
	netErr := errors.New(`git ls-remote: exit status 128 (output: fatal: unable to access: Could not resolve host: github.com)`)

	cases := []struct {
		name    string
		url     string
		fail    map[string]error
		want    string // "" means an error is expected
		wantErr int    // errs code when want == ""
	}{
		{name: "reachable as-is", url: httpsURL, fail: nil, want: httpsURL},
		{name: "auth fails, alt works", url: httpsURL,
			fail: map[string]error{httpsURL: authErr}, want: sshAlt},
		{name: "auth fails on both transports still adds", url: httpsURL,
			fail: map[string]error{httpsURL: authErr, sshAlt: authErr}, want: httpsURL},
		{name: "not found on both transports is refused", url: httpsURL,
			fail: map[string]error{httpsURL: notFound, sshAlt: notFound}, want: "", wantErr: errs.NotFound},
		{name: "not found over https but ssh works", url: httpsURL,
			fail: map[string]error{httpsURL: notFound}, want: sshAlt},
		{name: "network trouble never blocks", url: httpsURL,
			fail: map[string]error{httpsURL: netErr, sshAlt: netErr}, want: httpsURL},
		{name: "not found with no alternate transport is refused", url: "git://github.com/psf/requests",
			fail: map[string]error{"git://github.com/psf/requests": notFound}, want: "", wantErr: errs.NotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var probed []string
			got, err := preflightURL(tc.url, preflightProbe(tc.fail, &probed))
			if tc.want == "" {
				var coded *errs.Coded
				if !errors.As(err, &coded) || coded.Code != tc.wantErr {
					t.Fatalf("want errs code %d, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("preflightURL = %q, want %q (probed %v)", got, tc.want, probed)
			}
		})
	}
}

// The not-found refusal names the URL and the private-repo caveat: a private
// repo is reported as "not found" by GitHub, so the message must not assert
// the repo doesn't exist without pointing at auth as the other cause.
func TestPreflightURLNotFoundMessage(t *testing.T) {
	url := "https://github.com/psf/requets"
	probe := func(string) error { return errors.New("remote: Repository not found.") }
	_, err := preflightURL(url, probe)
	if err == nil {
		t.Fatal("want an error for a not-found repo")
	}
	if !strings.Contains(err.Error(), url) {
		t.Errorf("error should name the URL, got %q", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "private") {
		t.Errorf("error should mention the private-repo caveat, got %q", err)
	}
}

// Only the named owner's repos count: an owner with no repos still warns even
// when other owners have plenty, so one populated owner can't mask an empty one.
func TestOwnerEmptyHintScopedToOwner(t *testing.T) {
	c := &config.Config{
		Owners: []config.Owner{
			{URL: "https://github.com/acme"},
			{URL: "https://github.com/empty"},
		},
		Repos: []config.Repo{
			{URL: "https://github.com/acme/widget", Source: "github.com/acme"},
		},
	}
	if hint := ownerEmptyHint(c, "github.com/empty"); hint == "" {
		t.Fatal("an empty owner should warn even when another owner has repos")
	}
}
