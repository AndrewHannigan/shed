package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/AndrewHannigan/shed/pkg/agents"
)

// __welcome-tour prints the bundled tour script verbatim, newline-terminated,
// with no envelope or wrapping — the agent reads it as plain instructions.
func TestPrintWelcomeTour(t *testing.T) {
	var buf bytes.Buffer
	if err := printWelcomeTour(&buf); err != nil {
		t.Fatalf("printWelcomeTour: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, string(agents.TourContent)) {
		t.Errorf("output should start with the embedded tour script")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output should be newline-terminated")
	}
	// The script must actually walk the flow the tour promises, so a future
	// edit can't silently drop a step.
	for _, want := range []string{
		"AskUserQuestion",                      // checkpoint pacing
		"Run the `shed add` command",          // step 1
		"psf/requests",                         // single-repo example
		"read-only",                            // catalog is immutable
		"shed workspace new",                   // agent-driven workspaces
		"second workspace",                     // multiple workspaces demo
		"Don't open a PR",                      // explain, don't ship
		"typically run by the agent",           // workspace new is agent-driven
		"more background with less exposition", // real-world pacing
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tour script should mention %q", want)
		}
	}
	// The tour works on the user's real repos — a dummy demo repo must never
	// creep back in.
	if strings.Contains(out, "octocat") {
		t.Errorf("tour script should not use a dummy repo (found %q)", "octocat")
	}
}

// The session-context guide must advertise the tour so the agent knows to reach
// for it when the user asks for an intro.
func TestGuideAdvertisesWelcomeTour(t *testing.T) {
	if !strings.Contains(string(agents.DocContent), "shed __welcome-tour") {
		t.Errorf("guide should mention `shed __welcome-tour` so the agent can find it")
	}
}
