package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/worksource"
)

// --- hivecommons/hive#8124: the contributor relay's task prompt carries the
// assigning hive's project.writing_guide -------------------------------------
//
// ${WRITING_GUIDE} (#7667, landed in #7670) reaches every issue and PR a
// resident agent files through the default policy templates, but this prompt is
// built in Go and carried none of it — so an owner who set a guide got it on
// the quality lane's PRs and not on the PRs a contributor opened for the same
// repository.

const guideHeaderSentinel = "WRITING GUIDE (set by this hive's owner in project.writing_guide)"

func testWritingGuideSection(t *testing.T) string {
	t.Helper()
	p := &config.ProjectConfig{WritingGuide: "Short sentences. One idea per bullet."}
	section := p.WritingGuideSection()
	if section == "" {
		t.Fatalf("WritingGuideSection rendered nothing for a set guide")
	}
	return section
}

// The guide is rendered once, and in the position that is the whole point of
// the setting: next to the step that writes the body it governs. A style rule
// that arrives as background loses to the instruction sitting beside the task
// (#7667), so "somewhere in the prompt" is not good enough — it has to be after
// the checkout/base-branch mechanics and before the open-the-PR instruction.
func TestTaskPromptCarriesWritingGuideBeforeThePROpenStep(t *testing.T) {
	section := testWritingGuideSection(t)

	for _, tc := range []struct {
		name     string
		canPush  bool
		openStep string
	}{
		{"fork checkout", false, "Push your branch to your fork's 'origin' remote"},
		{"direct-push checkout", true, "Push your branch to the 'upstream' remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := buildTaskPromptForContributor(
				worksource.Ref{Repo: "acme/widgets", Number: 42}, "fix the thing", tc.canPush, section)

			if n := strings.Count(prompt, guideHeaderSentinel); n != 1 {
				t.Fatalf("expected the guide header exactly once, got %d:\n%s", n, prompt)
			}
			if !strings.Contains(prompt, "Short sentences. One idea per bullet.") {
				t.Fatalf("prompt does not carry the guide text:\n%s", prompt)
			}

			guideAt := strings.Index(prompt, guideHeaderSentinel)
			openAt := strings.Index(prompt, tc.openStep)
			draftAt := strings.Index(prompt, "Open it ready for review, not as a draft")
			branchAt := strings.Index(prompt, "git checkout -b <your-branch>")
			if openAt < 0 || draftAt < 0 || branchAt < 0 {
				t.Fatalf("prompt is missing an anchor (open=%d draft=%d branch=%d):\n%s",
					openAt, draftAt, branchAt, prompt)
			}
			if guideAt > openAt || guideAt > draftAt {
				t.Errorf("guide must precede the open-the-PR instruction (guide=%d open=%d draft=%d):\n%s",
					guideAt, openAt, draftAt, prompt)
			}
			if guideAt < branchAt {
				t.Errorf("guide should sit with the write-the-PR step, not above the checkout mechanics (guide=%d branch=%d):\n%s",
					guideAt, branchAt, prompt)
			}
		})
	}
}

// The repository's own AGENTS.md and CONTRIBUTING still win on format (#7159).
// The guide governs how the body READS; it is not licence to ignore the
// precedence paragraph, which must survive intact alongside it.
func TestTaskPromptWritingGuideKeepsRepoPrecedence(t *testing.T) {
	prompt := buildTaskPromptForContributor(
		worksource.Ref{Repo: "acme/widgets", Number: 42}, "fix the thing", true, testWritingGuideSection(t))

	for _, want := range []string{
		"Read the repository's own AGENTS.md and CONTRIBUTING before you start",
		"follow them wherever they conflict with these instructions",
		"It governs how the body reads",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lost %q:\n%s", want, prompt)
		}
	}
}

// An unset guide renders nothing, so a hive that never sets one gets the prompt
// it has always had — the same contract the template path states. The prompt is
// a single running paragraph with no newline anywhere in it; the guide block is
// the only thing that introduces one, so its absence is checkable directly.
func TestTaskPromptWithoutWritingGuideIsUnchanged(t *testing.T) {
	ref := worksource.Ref{Repo: "acme/widgets", Number: 42}

	for _, tc := range []struct {
		name  string
		guide string
	}{
		{"unset", ""},
		{"whitespace only", "   \n\t\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := buildTaskPromptForContributor(ref, "fix the thing", true, tc.guide)
			if strings.Contains(prompt, "WRITING GUIDE") {
				t.Errorf("empty guide rendered a guide section:\n%s", prompt)
			}
			if strings.ContainsRune(prompt, '\n') {
				t.Errorf("empty guide altered the prompt's shape (unexpected newline):\n%s", prompt)
			}
			if want := buildTaskPromptForContributor(ref, "fix the thing", true, ""); prompt != want {
				t.Errorf("prompt differs from the no-guide prompt:\ngot:  %q\nwant: %q", prompt, want)
			}
		})
	}
}

// A role-delegated task is still a contributor task: the guide belongs next to
// its open-the-PR step, not above the role preamble.
func TestRoleTaskPromptCarriesWritingGuide(t *testing.T) {
	section := testWritingGuideSection(t)
	prompt := buildRoleTaskPromptForContributor(
		worksource.Ref{Repo: "acme/widgets", Number: 42}, "fix the thing", "scanner", "", true, section)

	if n := strings.Count(prompt, guideHeaderSentinel); n != 1 {
		t.Fatalf("expected the guide header exactly once in the role prompt, got %d:\n%s", n, prompt)
	}
	if strings.Index(prompt, guideHeaderSentinel) > strings.Index(prompt, "Push your branch to the 'upstream' remote") {
		t.Errorf("role prompt places the guide after the open-the-PR step:\n%s", prompt)
	}
}

// writingGuideSection is nil-safe at every hop: the hub is constructed in tests
// without a full dependency graph, and a missing config means "no guide" rather
// than a panic on dispatch.
func TestHubWritingGuideSectionNilSafe(t *testing.T) {
	var h *ContributeWSHub
	if got := h.writingGuideSection(); got != "" {
		t.Fatalf("nil hub should render no guide, got %q", got)
	}
	if got := (&ContributeWSHub{}).writingGuideSection(); got != "" {
		t.Fatalf("hub without a server should render no guide, got %q", got)
	}
}

// End-to-end through selectTask: the shipped task_assign prompt carries the
// guide the hub owner configured — and carries nothing when they configured
// none.
func TestSelectTask_PromptCarriesConfiguredWritingGuide(t *testing.T) {
	t.Run("guide configured", func(t *testing.T) {
		hub, s := covK2Hub(t)
		oneActionableIssue(s)
		s.deps.Config.Project.WritingGuide = "Short sentences. One idea per bullet."

		c := &ContributorConnection{
			profile:    &ContributorProfile{GitHubUsername: "guide-user", ContributorID: "c-wg", TrustTier: "trusted"},
			cliBackend: "claude",
		}
		msg := hub.selectTask(c)
		if msg == nil || msg.Type != "task_assign" {
			t.Fatalf("expected task_assign, got %+v", msg)
		}
		if !strings.Contains(msg.Prompt, guideHeaderSentinel) {
			t.Fatalf("assignment prompt missing the writing-guide header:\n%s", msg.Prompt)
		}
		if !strings.Contains(msg.Prompt, "Short sentences. One idea per bullet.") {
			t.Fatalf("assignment prompt missing the configured guide text:\n%s", msg.Prompt)
		}
	})

	t.Run("no guide configured", func(t *testing.T) {
		hub, s := covK2Hub(t)
		oneActionableIssue(s)

		c := &ContributorConnection{
			profile:    &ContributorProfile{GitHubUsername: "noguide-user", ContributorID: "c-nwg", TrustTier: "trusted"},
			cliBackend: "claude",
		}
		msg := hub.selectTask(c)
		if msg == nil || msg.Type != "task_assign" {
			t.Fatalf("expected task_assign, got %+v", msg)
		}
		if strings.Contains(msg.Prompt, "WRITING GUIDE") {
			t.Fatalf("hive with no guide shipped a guide section:\n%s", msg.Prompt)
		}
	})
}
