package github

import (
	"errors"
	"testing"
)

// CheckPRRepoPolicy is the exported face of the pr-request watcher's PR repo
// policy gate (#9614). These pin its contract: no gate or no client means no
// refusal, an installed gate's refusal is returned verbatim with the request's
// agent and repo, and clearing the gate with nil removes the refusal.
func TestCheckPRRepoPolicy(t *testing.T) {
	var nilClient *Client
	if err := nilClient.CheckPRRepoPolicy("quality", "acme/widgets"); err != nil {
		t.Fatalf("nil client: got %v, want nil", err)
	}

	c := &Client{}
	if err := c.CheckPRRepoPolicy("quality", "acme/widgets"); err != nil {
		t.Fatalf("no gate installed: got %v, want nil", err)
	}

	errForbidden := errors.New("repo policy forbids PRs")
	var gotAgent, gotRepo string
	c.SetPRRepoPolicyGate(func(agent, repo string) error {
		gotAgent, gotRepo = agent, repo
		if repo == "acme/locked" {
			return errForbidden
		}
		return nil
	})
	if err := c.CheckPRRepoPolicy("quality", "acme/locked"); !errors.Is(err, errForbidden) {
		t.Fatalf("gate refusal: got %v, want %v", err, errForbidden)
	}
	if gotAgent != "quality" || gotRepo != "acme/locked" {
		t.Fatalf("gate saw (%q, %q), want (quality, acme/locked)", gotAgent, gotRepo)
	}
	if err := c.CheckPRRepoPolicy("quality", "acme/open"); err != nil {
		t.Fatalf("gate allows acme/open: got %v, want nil", err)
	}

	c.SetPRRepoPolicyGate(nil)
	if err := c.CheckPRRepoPolicy("quality", "acme/locked"); err != nil {
		t.Fatalf("gate cleared: got %v, want nil", err)
	}
}
