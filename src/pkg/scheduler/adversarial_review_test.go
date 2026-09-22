package scheduler

import (
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
	return pr
}

func TestReviewerReviewModelsBucketsAvoidAuthorModel(t *testing.T) {
	s := adversarialReviewerScheduler("", false)
	msgs := s.BuildKickMessages(adversarialActionable(
		adversarialPR(1, "gpt-5.6-terra"),
		adversarialPR(2, "gemini-3.7-flash"),
	), []string{"reviewer"})
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want one per review model bucket", len(msgs))
	}
	for _, msg := range msgs {
		if msg.ModelOverride == "" {
			t.Fatalf("missing model override: %#v", msg)
		}
		if strings.Contains(msg.Message, "author_model="+msg.ModelOverride) {
			t.Fatalf("review model %q was paired with same author model in message:\n%s", msg.ModelOverride, msg.Message)
		}
		if !strings.Contains(msg.Message, "invented APIs") || !strings.Contains(msg.Message, "author_model=") {
			t.Fatalf("independent review prompt missing author/failure-mode text:\n%s", msg.Message)
		}
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
		wantMsgs int
		wantMark string
	}{
		{"pinned", config.ReviewModelsFallbackPinned, 1, ""},
		{"skip", config.ReviewModelsFallbackSkip, 1, ""},
		{"requires_human", config.ReviewModelsFallbackRequiresHuman, 1, "needs human review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := adversarialReviewerScheduler(tc.fallback, false)
			s.cfg.Agents["reviewer"] = config.AgentConfig{
				Backend: "copilot", Model: "claude-fable-5", KickTemplate: "reviewer-queue.md",
				ReviewModels: config.ReviewModelsConfig{Pool: []config.ReviewModelPoolEntry{{Backend: "copilot", Model: "gpt-5.6-terra"}}, Fallback: tc.fallback},
			}
			msgs := s.BuildKickMessages(adversarialActionable(adversarialPR(1, "gpt-5.6-terra")), []string{"reviewer"})
			if len(msgs) != tc.wantMsgs {
				t.Fatalf("messages = %d, want %d", len(msgs), tc.wantMsgs)
			}
			if tc.wantMark != "" && !strings.Contains(msgs[0].Message, tc.wantMark) {
				t.Fatalf("message missing %q:\n%s", tc.wantMark, msgs[0].Message)
			}
			if tc.fallback == config.ReviewModelsFallbackSkip && strings.Contains(msgs[0].Message, "console#1") {
				t.Fatalf("skip fallback left the PR in the kick:\n%s", msgs[0].Message)
			}
		})
	}
}
