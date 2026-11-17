package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func topicLookup(claims ...TopicClaim) TopicClaimLookup {
	return func() []TopicClaim { return claims }
}

func TestHeldPRBlockAndKickShowTopicClaims(t *testing.T) {
	s := heldPRCoordinationScheduler(t, 3, "ISSUES_AND_PRS")
	s.SetTopicClaimLookup(topicLookup(
		TopicClaim{Agent: "quality", Key: "deps-bump", Repo: "acme/widget", PR: 26, Branch: "deps/renamed"},
		TopicClaim{Agent: "scanner", Key: "release", Repo: "acme/widget", PR: 40},
	))
	actionable := &github.ActionableResult{
		Hold: github.HoldResult{Total: 1, PRs: 1, Items: []github.HoldItem{
			{Repo: "acme/widget", Number: 26, Type: "pr", Title: "Bump deps"},
		}},
	}
	msg := s.BuildAgentMessage("quality", nil, actionable)
	for _, want := range []string{
		"acme/widget#26 [topic: deps-bump] Bump deps",
		topicClaimsHeader,
		"deps-bump → acme/widget#26 (branch deps/renamed)",
		"bd topic claim <key>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("kick missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "release → acme/widget#40") {
		t.Errorf("another lane's topic claim leaked into the kick:\n%s", msg)
	}

	s.SetTopicClaimLookup(nil)
	msg = s.BuildAgentMessage("quality", nil, actionable)
	if strings.Contains(msg, topicClaimsHeader) || strings.Contains(msg, "[topic:") {
		t.Errorf("kick carries topic claims with no lookup installed:\n%s", msg)
	}
}

func TestTopicClaimsTemplateVariable(t *testing.T) {
	redirectPolicySeams(t)
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "deps.md"), []byte("DEPS BODY\n${TOPIC_CLAIMS}\nEND BODY"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme", Name: "Acme", PrimaryRepo: "widget", Repos: []string{"acme/widget"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{
			"deps": {Role: "deps", Mode: "ISSUES_AND_PRS", KickTemplate: "deps.md"},
		}},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	s := New(cfg, testLogger())

	msg := s.BuildAgentMessage("deps", nil, &github.ActionableResult{})
	if !strings.Contains(msg, "You hold no topic claims.") || !strings.Contains(msg, "bd topic claim") {
		t.Fatalf("${TOPIC_CLAIMS} did not render the empty block:\n%s", msg)
	}

	s.SetTopicClaimLookup(topicLookup(TopicClaim{Agent: "deps", Key: "deps-bump", Repo: "acme/widget", PR: 7}))
	msg = s.BuildAgentMessage("deps", nil, &github.ActionableResult{})
	if got := strings.Count(msg, topicClaimsHeader); got != 1 {
		t.Fatalf("topic block rendered %d times, want 1 (the seam must not repeat a placed variable):\n%s", got, msg)
	}
	if !strings.Contains(msg, "deps-bump → acme/widget#7\n") {
		t.Fatalf("claim missing from ${TOPIC_CLAIMS}:\n%s", msg)
	}
	if strings.Index(msg, topicClaimsHeader) > strings.Index(msg, "END BODY") {
		t.Fatalf("block not placed where the template put it:\n%s", msg)
	}
}

func TestRedPRFixBlockTagsTopic(t *testing.T) {
	data := []byte(`{"ci_failing":[{"number":5,"repo":"acme/widget","title":"Bump deps","agent":"scanner","failing_checks":["build"]},
{"number":6,"repo":"acme/widget","title":"Other","agent":"scanner","failing_checks":["build"]}]}`)
	out := formatRedPRFixDataWithTopics(data, "scanner", nil, nil, []TopicClaim{{Agent: "scanner", Key: "deps-bump", Repo: "widget", PR: 5}})
	if !strings.Contains(out, "#5 acme/widget [topic: deps-bump] — Bump deps") {
		t.Fatalf("FIX-BEFORE-NEW row not tagged with its topic:\n%s", out)
	}
	if !strings.Contains(out, "#6 acme/widget — Other") {
		t.Fatalf("unclaimed row changed:\n%s", out)
	}
	if plain := formatRedPRFixDataWithSignals(data, "scanner", nil); strings.Contains(plain, "[topic:") {
		t.Fatalf("untagged renderer carries a topic:\n%s", plain)
	}
}

func TestTopicClaimHelpers(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"acme/widget", "ACME/Widget", true},
		{"acme/widget", "other/widget", false},
		{"widget", "acme/widget", true},
		{"widget", "gadget", false},
		{"", "widget", false},
	}
	for _, tc := range cases {
		if got := sameRepoSpelling(tc.a, tc.b); got != tc.want {
			t.Errorf("sameRepoSpelling(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	claims := []TopicClaim{{Key: "k", Repo: "acme/widget", PR: 3}}
	if got := topicTag(claims, "acme/widget", 3); got != " [topic: k]" {
		t.Errorf("topicTag = %q", got)
	}
	if got := topicTag(claims, "acme/widget", 4); got != "" {
		t.Errorf("topicTag on unclaimed PR = %q", got)
	}

	s := &Scheduler{}
	if got := s.addTopicClaims("deps", ""); got != "" {
		t.Errorf("empty message grew: %q", got)
	}
	msg := "BODY\n" + topicClaimsHeader + "\n"
	s.SetTopicClaimLookup(topicLookup(TopicClaim{Agent: "deps", Key: "k", Repo: "acme/widget", PR: 3}))
	if got := s.addTopicClaims("deps", msg); got != msg {
		t.Errorf("seam repeated a placed block: %q", got)
	}
}

// A pending claim from a sandboxed kick (hivecommons/hive#18572) has no PR
// yet; the kick says so instead of printing #0, and teaches hive-topic.
func TestTopicClaimsSectionShowsPendingClaim(t *testing.T) {
	got := topicClaimsSection([]TopicClaim{{Agent: "deps", Key: "bump", Repo: "acme/widget", Branch: "hive/deps-1"}})
	for _, want := range []string{"bump → acme/widget, PR not opened yet (pending on branch hive/deps-1)", "hive-topic"} {
		if !strings.Contains(got, want) {
			t.Errorf("section missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "#0") {
		t.Errorf("pending claim printed as #0:\n%s", got)
	}
}
