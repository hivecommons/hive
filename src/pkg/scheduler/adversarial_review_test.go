package scheduler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func adversarialReviewerScheduler(fallback string, excludeFamily bool) *Scheduler {
	s := newScheduler()
	if fallback == "" {
		fallback = config.ReviewModelsFallbackPinned
	}
	excludeModel := true
	s.cfg.Agents = map[string]config.AgentConfig{
		"reviewer": {
			Backend:      "copilot",
			Model:        "claude-fable-5",
			KickTemplate: "reviewer-queue.md",
			ReviewModels: config.ReviewModelsConfig{
				Pool: []config.ReviewModelPoolEntry{
					{Backend: "copilot", Model: "gpt-5.6-terra"},
					{Backend: "copilot", Model: "gemini-3.7-flash"},
					{Backend: "copilot", Model: "claude-opus-4-6"},
				},
				ExcludeAuthorModel:  &excludeModel,
				ExcludeAuthorFamily: excludeFamily,
				Fallback:            fallback,
			},
		},
	}
	return s
}

func adversarialActionable(prs ...github.PullRequest) *github.ActionableResult {
	return &github.ActionableResult{PRs: github.PRResult{Items: prs, Count: len(prs)}}
}

func adversarialPR(n int, model string) github.PullRequest {
	pr := makePR("console", n, "change", "bot")
	pr.HiveAttributed = true
	pr.HiveBackend = "copilot"
	pr.HiveModel = model
	pr.URL = fmt.Sprintf("https://github.com/test-org/console/pull/%d", n)
	return pr
}

func TestReviewerReviewModelsSingleKickAnnotatesIndependentModels(t *testing.T) {
	s := adversarialReviewerScheduler("", false)
	msgs := s.BuildKickMessages(adversarialActionable(
		adversarialPR(1, "gpt-5.6-terra"),
		adversarialPR(2, "gemini-3.7-flash"),
	), []string{"reviewer"})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want one mixed-queue reviewer kick", len(msgs))
	}
	msg := msgs[0].Message
	if !strings.Contains(msg, "## Independent review") || !strings.Contains(msg, "sub-agent (Agent tool)") || !strings.Contains(msg, "review_model") {
		t.Fatalf("independent review section missing sub-agent instructions:\n%s", msg)
	}
	for _, forbidden := range []string{"author=gpt-5.6-terra review_with=copilot/gpt-5.6-terra", "author=gemini-3.7-flash review_with=copilot/gemini-3.7-flash"} {
		if strings.Contains(msg, forbidden) {
			t.Fatalf("same author/reviewer model pairing rendered: %s\n%s", forbidden, msg)
		}
	}
	for _, want := range []string{"[author=gpt-5.6-terra review_with=copilot/gemini-3.7-flash]", "[author=gemini-3.7-flash review_with=copilot/gpt-5.6-terra]"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestReviewerIndependentReviewSectionOnlyWhenConfigured(t *testing.T) {
	s := newScheduler()
	s.cfg.Agents = map[string]config.AgentConfig{"reviewer": {Backend: "copilot", Model: "claude-fable-5", KickTemplate: "reviewer-queue.md"}}
	msgs := s.BuildKickMessages(adversarialActionable(adversarialPR(1, "gpt-5.6-terra")), []string{"reviewer"})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if strings.Contains(msgs[0].Message, "## Independent review") || strings.Contains(msgs[0].Message, "review_with=") {
		t.Fatalf("unconfigured reviewer got independent-review text:\n%s", msgs[0].Message)
	}
}

func TestReviewerManualKickIncludesIndependentReviewInstructions(t *testing.T) {
	s := adversarialReviewerScheduler("", false)
	s.SetLastActionable(adversarialActionable(adversarialPR(1, "gpt-5.6-terra")))
	msg := s.BuildAgentMessageFromLastActionable("reviewer")
	if !strings.Contains(msg, "## Independent review") || !strings.Contains(msg, "review_with=copilot/gemini-3.7-flash") {
		t.Fatalf("manual kick missing independent-review instructions or annotation:\n%s", msg)
	}
}

func TestReviewerReviewModelsFamilyExclusion(t *testing.T) {
	s := adversarialReviewerScheduler("", true)
	_, model, fallback := s.selectReviewModel(s.cfg.Agents["reviewer"], adversarialPR(1, "gpt-5.6-luna"))
	if fallback || model != "gemini-3.7-flash" {
		t.Fatalf("selectReviewModel = (%q,%v), want gemini family escape", model, fallback)
	}
}

func TestReviewerReviewModelsFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fallback string
		wantMark string
	}{
		{"pinned", config.ReviewModelsFallbackPinned, "review_with=copilot/claude-fable-5"},
		{"skip", config.ReviewModelsFallbackSkip, ""},
		{"requires_human", config.ReviewModelsFallbackRequiresHuman, "needs human review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := adversarialReviewerScheduler(tc.fallback, false)
			s.cfg.Agents["reviewer"] = config.AgentConfig{
				Backend: "copilot", Model: "claude-fable-5", KickTemplate: "reviewer-queue.md",
				ReviewModels: config.ReviewModelsConfig{Pool: []config.ReviewModelPoolEntry{{Backend: "copilot", Model: "gpt-5.6-terra"}}, Fallback: tc.fallback},
			}
			msgs := s.BuildKickMessages(adversarialActionable(adversarialPR(1, "gpt-5.6-terra")), []string{"reviewer"})
			if len(msgs) != 1 {
				t.Fatalf("messages = %d, want 1", len(msgs))
			}
			if tc.wantMark != "" && !strings.Contains(msgs[0].Message, tc.wantMark) {
				t.Fatalf("message missing %q:\n%s", tc.wantMark, msgs[0].Message)
			}
			if tc.fallback == config.ReviewModelsFallbackRequiresHuman && strings.Contains(msgs[0].Message, "[author=gpt-5.6-terra review_with=") {
				t.Fatalf("requires_human fallback should not delegate to a model:\n%s", msgs[0].Message)
			}
			if tc.fallback == config.ReviewModelsFallbackSkip && strings.Contains(msgs[0].Message, "console#1") {
				t.Fatalf("skip fallback left the PR in the kick:\n%s", msgs[0].Message)
			}
		})
	}
}
