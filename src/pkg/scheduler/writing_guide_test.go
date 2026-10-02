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
	out, _ := s.substituteTemplateWithPolicy(string(data), nil, "quality", nil)
	return out
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

// hivecommons/hive#9747: a guide that asks for a plain-language summary first
// got a body that was the template and nothing else, because the preamble both
// granted the guide "structure" and pinned every template section. The kick an
// agent receives with that guide must say, before the body template, that a
// leading summary is allowed as an addition and that every template section
// still follows in the template's order.
func TestKick_SummaryFirstGuideAddsSummaryAndKeepsTemplateOrder(t *testing.T) {
	const guide = "People who are not in your head will read this. Write for them. Start by explaining at a high level. " +
		"The top part of the issue/PR should be easy to read. You can put the technical stuff and required items in a second section afterwards."
	out := writingGuideKick(t, guide)

	headerAt := strings.Index(out, "WRITING GUIDE (set by this hive's owner")
	guideAt := strings.Index(out, guide)
	bodyAt := strings.Index(out, `--body "## Finding`)
	if headerAt < 0 || guideAt < 0 || bodyAt < 0 {
		t.Fatalf("kick is missing the guide header (@%d), the guide text (@%d) or the body template (@%d):\n%s", headerAt, guideAt, bodyAt, out)
	}
	if !(headerAt < guideAt && guideAt < bodyAt) {
		t.Fatalf("want header < guide < body template, got %d, %d, %d", headerAt, guideAt, bodyAt)
	}
	preamble := out[headerAt:guideAt]
	for _, want := range []string{
		"If the guide asks for a summary or overview, write it at the top, before the template's first section, as an addition",
		"the template's sections still follow in full below it",
		"in the template's order",
		"never drop, rename or reorder a template section",
	} {
		if !strings.Contains(preamble, want) {
			t.Errorf("preamble ahead of the summary-first guide is missing %q:\n%s", want, preamble)
		}
	}
}

// hivecommons/hive#9926: with the #9747 fix live, agents did add the summary
// but still wrote it — and the title — for someone who already knew the code,
// so the opening named internal parts and never said why a user should care.
// The kick must spell the audience out: the title and the first paragraph are
// for a newcomer, the technical detail stays in the sections below, and the
// preamble outranks the style of the examples and recalled work it ships with.
func TestKick_WritingGuideDemandsNewcomerReadableTitleAndOpening(t *testing.T) {
	out := writingGuideKick(t, "Write for people who are not in your head.")

	for _, want := range []string{
		"Write that opening paragraph and the title for a newcomer who does not know this codebase",
		"say what the thing is, what the problem or change is, and why it matters to a user, in plain words",
		"with no internal names, file paths or jargon left unexplained",
		"The technical detail then follows unchanged in the template's sections below",
		"this preamble outranks the style of any example or past issue/PR you were shown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kick is missing %q — agents keep writing the summary and title for insiders without it", want)
		}
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
