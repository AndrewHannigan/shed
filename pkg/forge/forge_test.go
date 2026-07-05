package forge

import (
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

func TestBuildListArgs(t *testing.T) {
	tests := []struct {
		name  string
		login string
		f     Filter
		want  []string
	}{
		{
			name:  "defaults exclude forks and archived",
			login: "octocat",
			f:     Filter{},
			want: []string{"repo", "list",
				"--limit", "1000",
				"--json", "name,url,sshUrl,isFork,isArchived,visibility",
				"--source", "--no-archived",
				"--", "octocat"},
		},
		{
			name:  "include everything, custom limit, private only",
			login: "acme",
			f:     Filter{IncludeForks: true, IncludeArchived: true, Visibility: "private", Limit: 5},
			want: []string{"repo", "list",
				"--limit", "5",
				"--json", "name,url,sshUrl,isFork,isArchived,visibility",
				"--visibility", "private",
				"--", "acme"},
		},
		{
			name:  "visibility all is omitted",
			login: "acme",
			f:     Filter{IncludeForks: true, IncludeArchived: true, Visibility: "all"},
			want: []string{"repo", "list",
				"--limit", "1000",
				"--json", "name,url,sshUrl,isFork,isArchived,visibility",
				"--", "acme"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildListArgs(tt.login, tt.f)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("buildListArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecodeRepos(t *testing.T) {
	data := []byte(`[
		{"name":"alpha","url":"https://github.com/acme/alpha","sshUrl":"git@github.com:acme/alpha.git","isFork":false,"isArchived":false,"visibility":"PUBLIC"},
		{"name":"beta","url":"https://github.com/acme/beta","sshUrl":"git@github.com:acme/beta.git","isFork":true,"isArchived":false,"visibility":"PRIVATE"}
	]`)

	t.Run("https clone urls", func(t *testing.T) {
		repos, err := decodeRepos(data, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(repos) != 2 {
			t.Fatalf("got %d repos, want 2", len(repos))
		}
		if repos[0].Name != "alpha" || repos[0].CloneURL != "https://github.com/acme/alpha" {
			t.Fatalf("unexpected first repo: %+v", repos[0])
		}
		if !repos[1].IsFork || repos[1].Visibility != "PRIVATE" {
			t.Fatalf("unexpected second repo: %+v", repos[1])
		}
	})

	t.Run("ssh clone urls", func(t *testing.T) {
		repos, err := decodeRepos(data, true)
		if err != nil {
			t.Fatal(err)
		}
		if repos[0].CloneURL != "git@github.com:acme/alpha.git" {
			t.Fatalf("want ssh url, got %q", repos[0].CloneURL)
		}
	})

	t.Run("malformed json errors", func(t *testing.T) {
		if _, err := decodeRepos([]byte("not json"), false); err == nil {
			t.Fatal("expected error for malformed json")
		}
	})
}

func TestClassifyExecErr(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		stderr string
		want   error // sentinel to errors.Is against; nil means "neither sentinel"
	}{
		{"binary missing", exec.ErrNotFound, "", ErrGhMissing},
		{"not logged in", errors.New("exit status 1"), "To get started with GitHub CLI, please run: gh auth login", ErrGhUnauthed},
		{"requires auth", errors.New("exit status 1"), "HTTP 401: requires authentication", ErrGhUnauthed},
		{"other failure", errors.New("exit status 1"), "could not resolve host", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyExecErr("gh repo list", tt.err, tt.stderr)
			if got == nil {
				t.Fatal("classifyExecErr returned nil")
			}
			switch tt.want {
			case ErrGhMissing:
				if !errors.Is(got, ErrGhMissing) {
					t.Fatalf("want ErrGhMissing, got %v", got)
				}
			case ErrGhUnauthed:
				if !errors.Is(got, ErrGhUnauthed) {
					t.Fatalf("want ErrGhUnauthed, got %v", got)
				}
			default:
				if errors.Is(got, ErrGhMissing) || errors.Is(got, ErrGhUnauthed) {
					t.Fatalf("want a non-sentinel error, got %v", got)
				}
			}
		})
	}
}

func TestBuildOwnerCheckArgs(t *testing.T) {
	got := buildOwnerCheckArgs("octocat")
	want := []string{"api", "users/octocat"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildOwnerCheckArgs() = %v, want %v", got, want)
	}
	// A login starting with "-" must stay a path segment, never a flag.
	if got := buildOwnerCheckArgs("-x"); got[len(got)-1] != "users/-x" {
		t.Fatalf("dash login should be prefixed, got %v", got)
	}
}

func TestClassifyOwnerCheck(t *testing.T) {
	t.Run("success means exists", func(t *testing.T) {
		exists, err := classifyOwnerCheck(nil, "")
		if !exists || err != nil {
			t.Fatalf("nil run error should mean exists, got exists=%v err=%v", exists, err)
		}
	})

	// gh prints "gh: Not Found (HTTP 404)" for an unknown account; that's
	// "doesn't exist", not a failure to surface.
	for _, stderr := range []string{"gh: Not Found (HTTP 404)", "HTTP 404: Not Found"} {
		t.Run("404 means absent: "+stderr, func(t *testing.T) {
			exists, err := classifyOwnerCheck(errors.New("exit status 1"), stderr)
			if exists || err != nil {
				t.Fatalf("404 should mean absent with no error, got exists=%v err=%v", exists, err)
			}
		})
	}

	t.Run("auth failure surfaces as sentinel", func(t *testing.T) {
		exists, err := classifyOwnerCheck(errors.New("exit status 1"), "HTTP 401: requires authentication")
		if exists {
			t.Fatal("auth failure should not report the owner as existing")
		}
		if !errors.Is(err, ErrGhUnauthed) {
			t.Fatalf("want ErrGhUnauthed, got %v", err)
		}
	})

	// A transient/unknown failure must be an error, never silently "absent" —
	// otherwise a flaky network would delete-by-omission a real owner.
	t.Run("other failure is a real error", func(t *testing.T) {
		exists, err := classifyOwnerCheck(errors.New("exit status 1"), "could not resolve host")
		if exists {
			t.Fatal("unknown failure should not report the owner as existing")
		}
		if err == nil {
			t.Fatal("unknown failure should surface an error, not nil")
		}
		if errors.Is(err, ErrGhUnauthed) || errors.Is(err, ErrGhMissing) {
			t.Fatalf("want a non-sentinel error, got %v", err)
		}
	})
}

func TestBuildPRListArgs(t *testing.T) {
	got := buildPRListArgs("acme/widgets", "feature/login")
	want := []string{
		"pr", "list",
		"--repo=acme/widgets",
		"--head=feature/login",
		"--state", "merged",
		"--json", "number",
		"--limit", "1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildPRListArgs() = %v, want %v", got, want)
	}
}

func TestDecodeMergedPR(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    int
		wantErr bool
	}{
		{"merged", `[{"number":42}]`, 42, false},
		{"none", `[]`, 0, false},
		{"garbage", `not json`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeMergedPR([]byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeMergedPR() err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("decodeMergedPR() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBuildPRViewArgs(t *testing.T) {
	got := buildPRViewArgs("acme/widget", 123)
	want := []string{"pr", "view", "123",
		"--repo=acme/widget",
		"--json", "number,title,state,headRefName,isCrossRepository,headRepository,headRepositoryOwner"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildPRViewArgs() = %v, want %v", got, want)
	}
}

func TestDecodePR(t *testing.T) {
	t.Run("same-repo PR", func(t *testing.T) {
		data := []byte(`{
			"number": 42,
			"title": "Fix the flux capacitor",
			"state": "OPEN",
			"headRefName": "fix-flux",
			"isCrossRepository": false,
			"headRepository": {"name": "widget"},
			"headRepositoryOwner": {"login": "acme"}
		}`)
		got, err := decodePR(data)
		if err != nil {
			t.Fatalf("decodePR: %v", err)
		}
		want := PR{Number: 42, Title: "Fix the flux capacitor", State: "OPEN",
			HeadRefName: "fix-flux", CrossRepo: false, HeadOwner: "acme", HeadName: "widget"}
		if got != want {
			t.Fatalf("decodePR = %+v, want %+v", got, want)
		}
	})

	t.Run("cross-repo PR from a fork", func(t *testing.T) {
		data := []byte(`{
			"number": 7,
			"title": "typo",
			"state": "MERGED",
			"headRefName": "patch-1",
			"isCrossRepository": true,
			"headRepository": {"name": "widget"},
			"headRepositoryOwner": {"login": "contributor"}
		}`)
		got, err := decodePR(data)
		if err != nil {
			t.Fatalf("decodePR: %v", err)
		}
		if !got.CrossRepo || got.HeadOwner != "contributor" || got.State != "MERGED" {
			t.Fatalf("decodePR = %+v, want cross-repo from contributor, MERGED", got)
		}
	})

	// gh emits null for headRepository/headRepositoryOwner when the fork was
	// deleted; decoding must not fail, just leave the head fields empty.
	t.Run("deleted fork yields empty head fields", func(t *testing.T) {
		data := []byte(`{
			"number": 9,
			"title": "orphan",
			"state": "OPEN",
			"headRefName": "gone",
			"isCrossRepository": true,
			"headRepository": null,
			"headRepositoryOwner": null
		}`)
		got, err := decodePR(data)
		if err != nil {
			t.Fatalf("decodePR: %v", err)
		}
		if got.HeadOwner != "" || got.HeadName != "" {
			t.Fatalf("decodePR = %+v, want empty head owner/name", got)
		}
	})

	t.Run("malformed JSON is an error", func(t *testing.T) {
		if _, err := decodePR([]byte("not json")); err == nil {
			t.Fatal("decodePR should fail on malformed input")
		}
	})
}

func TestForkCloneURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"https base gives https fork", "https://github.com/acme/widget", "https://github.com/contrib/widget"},
		{"scp-style ssh base gives ssh fork", "git@github.com:acme/widget.git", "git@github.com:contrib/widget.git"},
		{"ssh scheme base gives ssh fork", "ssh://git@github.com/acme/widget.git", "git@github.com:contrib/widget.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ForkCloneURL(tt.baseURL, "github.com", "contrib", "widget")
			if got != tt.want {
				t.Fatalf("ForkCloneURL(%q) = %q, want %q", tt.baseURL, got, tt.want)
			}
		})
	}
}
