package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#18199: which repos the remote-item pass watches and how a
// marker's agent name maps to a configured agent.
func TestRemoteItemRepos(t *testing.T) {
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs"}, // read-only
		{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395", CredentialState: config.ReferenceCredentialReadOnly},
		{Repo: "hivecommons/hive", Write: []string{"issues", "prs"}, CredentialState: config.ReferenceCredentialWriteTier, CredentialLogin: "nate-double-u"},
		{Repo: "hivecommons/site", Write: []string{"issues"}, DerivedFrom: "cncf/techdocs#395", CredentialState: config.ReferenceCredentialWriteTier, CredentialLogin: "nate-double-u"},
	}
	repos, login := remoteItemRepos(refs)
	if len(repos) != 2 || repos[0] != "hivecommons/hive" || repos[1] != "hivecommons/site" || login != "nate-double-u" {
		t.Fatalf("repos=%v login=%q", repos, login)
	}
	if repos, login := remoteItemRepos(refs[:2]); len(repos) != 0 || login != "" {
		t.Fatalf("read-only and downgraded entries must not be watched: %v %q", repos, login)
	}
}

func TestResolveMarkerAgent(t *testing.T) {
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{
		"docs-assessor": {DisplayName: "Docs Assessor"},
		"scanner":       {},
	}}}
	for in, want := range map[string]string{
		"docs-assessor": "docs-assessor",
		"Docs Assessor": "docs-assessor",
		"docs assessor": "docs-assessor",
		"SCANNER":       "scanner",
		"ghost":         "",
		"":              "",
		" scanner ":     "scanner",
	} {
		if got := resolveMarkerAgent(cfg, in); got != want {
			t.Errorf("resolveMarkerAgent(%q) = %q, want %q", in, got, want)
		}
	}
	if resolveMarkerAgent(nil, "scanner") != "" {
		t.Error("nil config resolves nothing")
	}
}

// The runtime only builds a poller when an owner token source is wired, and
// the pass is a no-op without one or without watched repos.
func TestNewRemoteItemsPollerNeedsOwnerToken(t *testing.T) {
	b := &boot{cfg: &config.Config{}, logger: testLogger()}
	if b.newRemoteItemsPoller(&referenceRepoRuntime{}) != nil {
		t.Fatal("no owner token source: no poller")
	}
	rt := &referenceRepoRuntime{deps: testReferenceRepoDeps()}
	p := b.newRemoteItemsPoller(rt)
	if p == nil || p.Fetch == nil || p.Marker == nil || p.ResolveAgent == nil {
		t.Fatalf("poller incomplete: %+v", p)
	}
	b.pollRemoteItems(nil, rt, nil)                              //nolint:staticcheck // nil ctx is the guarded case
	b.pollRemoteItems(t.Context(), nil, nil)                     // nil runtime
	b.pollRemoteItems(t.Context(), rt, []config.ReferenceRepo{}) // nothing to watch
}
