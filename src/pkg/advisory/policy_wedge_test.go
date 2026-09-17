package advisory

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
)

// The digest build/post predicates used to take a *github.Client that they only
// ever nil-checked. It is a bool here (see the policy.go package comment: this
// package cannot import pkg/github without a cycle), so the cases that used to
// pass a client now pass true and the cases that passed nil now pass false.

func TestPrimaryAdvisoryRepo(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"nil config", nil, ""},
		{"primary wins", &config.Config{Project: config.ProjectConfig{PrimaryRepo: "org/a", Repos: []string{"org/b"}}}, "org/a"},
		{"falls back to first repo", &config.Config{Project: config.ProjectConfig{Repos: []string{"org/b", "org/c"}}}, "org/b"},
		{"nothing configured", &config.Config{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PrimaryRepo(tc.cfg); got != tc.want {
				t.Fatalf("primaryAdvisoryRepo = %q, want %q", got, tc.want)
			}
		})
	}
}

// The per-cycle re-ensure must fire for a repo that never resolved AND for one
// whose recorded number is the failed-ensure zero value — the state that left
// certus posting nowhere for six days (#4167).
func TestAdvisoryIssueUnresolved(t *testing.T) {
	issues := map[string]int{"org/zero": 0, "org/ok": 42}
	if !IssueUnresolved(issues, "org/missing") {
		t.Error("a repo with no entry must be treated as unresolved")
	}
	if !IssueUnresolved(issues, "org/zero") {
		t.Error("a recorded issue number of 0 must be treated as unresolved")
	}
	if IssueUnresolved(issues, "org/ok") {
		t.Error("a resolved issue number must not trigger a re-ensure")
	}
}

func TestEmptyAdvisoryDigestRequiresExistingIssueAndClient(t *testing.T) {
	if !ShouldBuildDigest(nil, true, true) {
		t.Fatal("empty digest should still be built for an existing advisory issue")
	}
	if ShouldBuildDigest(nil, true, false) {
		t.Fatal("empty digest must not create advisory participation from nothing")
	}
	if ShouldBuildDigest(nil, false, true) {
		t.Fatal("empty digest requires a GitHub client capable of attempting the write")
	}
	if !ShouldBuildDigest(map[string]*beads.Store{"scanner": nil}, false, false) {
		t.Fatal("non-empty store set should still build so missing-issue errors can be reported")
	}
}

func TestEmptyAdvisoryDigestPostsOnlyToExistingIssue(t *testing.T) {
	empty := &Digest{}
	withFinding := &Digest{TotalCount: 1}
	withResolved := &Digest{RecentlyResolved: []ResolvedFinding{{Title: "fixed"}}}

	if !ShouldPostDigest(empty, true, true) {
		t.Fatal("empty digest should post as a freshness marker when a pinned issue exists")
	}
	if ShouldPostDigest(empty, true, false) {
		t.Fatal("empty digest must not post without an existing pinned issue")
	}
	if ShouldPostDigest(empty, false, true) {
		t.Fatal("empty digest must not post without a GitHub client")
	}
	if !ShouldPostDigest(withFinding, false, false) {
		t.Fatal("findings must still flow to the missing-issue error path")
	}
	if !ShouldPostDigest(withResolved, false, false) {
		t.Fatal("recently resolved findings must still flow to the missing-issue error path")
	}
}

func TestAdvisoryIssueNumber(t *testing.T) {
	issues := map[string]int{"org/ok": 42, "org/zero": 0}

	if num, ok := IssueNumber(issues, "org/ok"); !ok || num != 42 {
		t.Errorf("IssueNumber(org/ok) = %d, %v; want 42, true", num, ok)
	}
	if _, ok := IssueNumber(issues, "org/zero"); ok {
		t.Error("IssueNumber(org/zero) reported ok for a recorded 0")
	}
	if _, ok := IssueNumber(issues, "org/missing"); ok {
		t.Error("IssueNumber(org/missing) reported ok for an absent repo")
	}
	if _, ok := IssueNumber(nil, "org/ok"); ok {
		t.Error("IssueNumber(nil map) reported ok")
	}
}
