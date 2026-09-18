package scheduler

// describeTemplateFallback names the template BuildAgentMessage would fall
// back to when no configured kick_template resolves. Its convention-template
// and hardcoded branches are pinned by TestResolveTemplate_ReportsDanglingAndFallback;
// these tests pin the ACMM-pack branch and its fall-throughs, which had no
// coverage: the branch is what makes the dashboard prompt editor report the
// pack template a kick will really use at a configured maturity level.

import "testing"

// acmmScheduler is danglingScheduler with the config pinned to an ACMM level.
func acmmScheduler(t *testing.T, level int) *Scheduler {
	t.Helper()
	s, _ := danglingScheduler(t)
	s.cfg.ACMMLevel = &level
	return s
}

func TestDescribeTemplateFallback_ACMMPackTemplateWins(t *testing.T) {
	s := acmmScheduler(t, 2)

	// "guide" is in the level-2 pack with kick_template guide-advisory.md,
	// which ships in pkg/policies/defaults — the pack branch must name it.
	got := s.describeTemplateFallback("guide")
	want := "ACMM level 2 pack template guide-advisory.md"
	if got != want {
		t.Errorf("describeTemplateFallback(guide) = %q, want %q", got, want)
	}
}

func TestDescribeTemplateFallback_AgentNotInPackFallsThrough(t *testing.T) {
	s := acmmScheduler(t, 2)

	// "review" is not in the level-2 pack roster and has no review.md
	// convention template in the embedded defaults, so with the policy dirs
	// pointed at empty temp dirs the chain must end at the hardcoded kick.
	got := s.describeTemplateFallback("review")
	if got != "hardcoded kick for review" {
		t.Errorf("describeTemplateFallback(review) = %q, want the hardcoded kick", got)
	}
}

func TestDescribeTemplateFallback_UnknownLevelFallsThrough(t *testing.T) {
	s := acmmScheduler(t, 99)

	// No pack exists for level 99: ACMMPackByLevel errors and the chain must
	// keep going. guide.md ships as an embedded convention template, so the
	// description lands there rather than on the hardcoded kick.
	got := s.describeTemplateFallback("guide")
	if got != "convention template guide.md" {
		t.Errorf("describeTemplateFallback(guide) at unknown level = %q, want the convention template", got)
	}
}

func TestDescribeTemplateFallback_LevelZeroSkipsThePackBranch(t *testing.T) {
	s := acmmScheduler(t, 0)

	// A configured level of 0 means "no pack": the branch is gated on
	// *ACMMLevel > 0, so guide must resolve to its convention template even
	// though a nil-safe pack lookup for level 0 does not exist.
	got := s.describeTemplateFallback("guide")
	if got != "convention template guide.md" {
		t.Errorf("describeTemplateFallback(guide) at level 0 = %q, want the convention template", got)
	}
}

func TestResolveTemplate_FallbackReportsThePackTemplate(t *testing.T) {
	s := acmmScheduler(t, 2)

	// The prompt editor consumes the fallback via ResolveTemplate: an agent
	// with no kick_template configured at level 2 must report the pack
	// template as what a kick will actually use.
	res := s.ResolveTemplate("quality")
	if res.Fallback != "ACMM level 2 pack template quality-advisory.md" {
		t.Errorf("ResolveTemplate(quality).Fallback = %q, want the level-2 pack template", res.Fallback)
	}
}
