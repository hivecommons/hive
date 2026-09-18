package scheduler

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/policies"
)

// hivecommons/hive#7667: project.writing_guide reaches the agent through the
// kick — as ${WRITING_GUIDE}, rendered ahead of the body template in every
// default policy that files an issue or PR. These drive the real embedded
// quality-full.md through substituteTemplate, the same path a kick takes, so
// the assertion is on what the agent is actually told.

func writingGuideKick(t *testing.T, guide string) string {
	t.Helper()
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme", Name: "widgets", Repos: []string{"widgets"}, WritingGuide: guide},
		Agents:  map[string]config.AgentConfig{"quality": {Role: "quality"}},
	}
	s := New(cfg, slog.Default())
	data, err := policies.DefaultPolicies.ReadFile("defaults/quality-full.md")
	if err != nil {
		t.Fatalf("read embedded quality-full.md: %v", err)
	}
	return s.substituteTemplate(string(data), nil, "quality", nil)
}

func TestKick_WritingGuideLandsAheadOfTheBodyTemplate(t *testing.T) {
	out := writingGuideKick(t, "A person who was not in your head will read this. Write for them.")

	if strings.Contains(out, "${WRITING_GUIDE}") {
		t.Fatal("${WRITING_GUIDE} was left literal in the kick")
	}
	guideAt := strings.Index(out, "A person who was not in your head will read this.")
	if guideAt < 0 {
		t.Fatalf("the owner's guide is not in the kick:\n%s", out)
	}
	bodyAt := strings.Index(out, `--body "## Finding`)
	if bodyAt < 0 {
		t.Fatal("quality-full.md no longer carries its issue body template; update this test's anchor")
	}
	if guideAt > bodyAt {
		t.Fatalf("the guide (@%d) must precede the body template it governs (@%d) — next to the template is the position that changed behaviour in #7667", guideAt, bodyAt)
	}
	if !strings.Contains(out, "WRITING GUIDE (set by this hive's owner") {
		t.Fatal("the guide must be introduced as the owner's instruction, not pasted bare")
	}
}

func TestKick_WritingGuideUnsetChangesNothing(t *testing.T) {
	out := writingGuideKick(t, "")
	for _, absent := range []string{"${WRITING_GUIDE}", "WRITING GUIDE"} {
		if strings.Contains(out, absent) {
			t.Fatalf("an unset guide must render nothing — found %q in the kick", absent)
		}
	}
}
