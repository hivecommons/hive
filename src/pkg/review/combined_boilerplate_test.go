package review

import (
	"fmt"
	"strings"
	"testing"
)

func combinedBoilerplatePR() PullRequest {
	return PullRequest{Repo: "o/r", Number: 7, Title: "t", HeadSHA: testSHA, MergeBase: "abc123", Author: "someone", ScopeContract: "linked issue o/r#1"}
}

func combinedBoilerplateOpts() PromptOptions {
	return PromptOptions{PostComments: true, ReviewBotLogins: []string{"chatgpt-codex-connector[bot]"}, WritingGuideSection: "WRITING GUIDE\n\nbe brief.\n"}
}

// The shared block — scope contract, read instructions, review-bot thread
// obligations, grounding, schema, publish contract — appears exactly once in a
// combined kick, however many perspectives it covers. That is the token saving
// the mode exists to deliver (hivecommons/hive#12126).
func TestCombinedPromptEmitsSharedBoilerplateOnce(t *testing.T) {
	got := BuildCombinedPrompt(combinedBoilerplatePR(), DefaultPerspectives, combinedBoilerplateOpts())
	for _, marker := range []string{
		"SHARED INSTRUCTIONS",
		"SCOPE CONTRACT",
		"READ THE PR BEFORE YOU JUDGE IT.",
		"REVIEW-BOT FINDINGS",
		"GROUNDING — read the code, do not infer it.",
		"Allowed verdicts:",
		"PUBLISH YOUR VERDICT.",
		"PERSPECTIVE SECTIONS",
		"WRITING GUIDE",
	} {
		if n := strings.Count(got, marker); n != 1 {
			t.Errorf("%q appears %d times in the combined kick, want exactly 1", marker, n)
		}
	}
}

// Every perspective gets its own section, after the shared block, and the
// sections carry only what differs: the focus line and the verdict object the
// perspective owes. The per-perspective verdict schema is preserved.
func TestCombinedPromptPerspectiveSectionsFollowSharedBlock(t *testing.T) {
	got := BuildCombinedPrompt(combinedBoilerplatePR(), DefaultPerspectives, combinedBoilerplateOpts())
	shared := strings.Index(got, "SHARED INSTRUCTIONS")
	sections := strings.Index(got, "PERSPECTIVE SECTIONS")
	publish := strings.Index(got, "PUBLISH YOUR VERDICT.")
	if shared < 0 || sections < 0 || publish < 0 {
		t.Fatalf("missing structural markers:\n%s", got)
	}
	if !(shared < publish && publish < sections) {
		t.Fatalf("want shared block, then publish contract, then perspective sections; got offsets shared=%d publish=%d sections=%d", shared, publish, sections)
	}
	for _, p := range DefaultPerspectives {
		header := fmt.Sprintf("[perspective:%s]", p)
		at := strings.Index(got, header)
		if at < 0 {
			t.Errorf("no section for %s", p)
			continue
		}
		if at < sections {
			t.Errorf("section for %s precedes the PERSPECTIVE SECTIONS heading", p)
		}
		if want := fmt.Sprintf("Verdict object: perspective %q", string(p)); !strings.Contains(got, want) {
			t.Errorf("section for %s does not name its verdict object", p)
		}
		if !strings.Contains(got, string(p)+" — "+(PerspectiveSet{}).Focus(p)) {
			t.Errorf("section for %s lacks its focus line", p)
		}
	}
	for _, want := range []string{
		"Return exactly one JSON ARRAY",
		fmt.Sprintf("all %d of them", len(DefaultPerspectives)),
		"Give each perspective its OWN verdict.",
		"Allowed perspective values: " + joinPerspectives(DefaultPerspectives),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("per-perspective verdict schema lost %q", want)
		}
	}
}

// The combined kick is materially smaller than the per-perspective kicks it
// replaces; otherwise the mode saves comments but not tokens.
func TestCombinedPromptIsSmallerThanPerPerspectiveKicks(t *testing.T) {
	pr, opts := combinedBoilerplatePR(), combinedBoilerplateOpts()
	combined := len(BuildCombinedPrompt(pr, DefaultPerspectives, opts))
	separate := 0
	for _, p := range DefaultPerspectives {
		separate += len(BuildPerspectivePromptWith(p, pr, opts))
	}
	// Five full kicks against one shared block plus five short sections:
	// anything above half the separate total means the boilerplate is being
	// repeated somewhere.
	if combined*2 > separate {
		t.Fatalf("combined kick is %d bytes against %d for %d separate kicks; shared boilerplate is not being emitted once", combined, separate, len(DefaultPerspectives))
	}
}

// The grounding budget is stated once for the session and grows with the
// number of perspectives, since the tree is read once and each extra
// perspective adds targeted lookups rather than another pass.
func TestCombinedGroundingBudgetScalesWithPerspectives(t *testing.T) {
	if got := combinedGroundingBudget(1); got != GroundingToolCallBudget {
		t.Fatalf("one perspective: budget %d, want %d", got, GroundingToolCallBudget)
	}
	n := len(DefaultPerspectives)
	want := GroundingToolCallBudget + (n-1)*GroundingToolCallBudgetPerExtraPerspective
	if got := combinedGroundingBudget(n); got != want {
		t.Fatalf("%d perspectives: budget %d, want %d", n, got, want)
	}
	if got := combinedGroundingBudget(0); got != GroundingToolCallBudget {
		t.Fatalf("zero perspectives: budget %d, want %d", got, GroundingToolCallBudget)
	}
	prompt := BuildCombinedPrompt(combinedBoilerplatePR(), DefaultPerspectives, PromptOptions{})
	line := fmt.Sprintf("up to %d investigation tool calls for this whole review, across all %d perspectives", want, n)
	if strings.Count(prompt, line) != 1 {
		t.Fatalf("combined kick does not state the session budget once:\n%s", prompt)
	}
	single := BuildPerspectivePrompt(PerspectiveCorrectness, combinedBoilerplatePR())
	if !strings.Contains(single, fmt.Sprintf("up to %d investigation tool calls for this perspective", GroundingToolCallBudget)) {
		t.Fatal("single kick lost its per-perspective budget line")
	}
}

// The review-bot thread contract survives in both kick shapes, and the fetch is
// bounded to what the reviewer acts on: the finding plus the first few replies.
func TestReviewBotThreadFetchIsBoundedAndContractPreserved(t *testing.T) {
	pr, opts := combinedBoilerplatePR(), combinedBoilerplateOpts()
	for name, got := range map[string]string{
		"single":   BuildPerspectivePromptWith(PerspectiveCorrectness, pr, opts),
		"combined": BuildCombinedPrompt(pr, DefaultPerspectives, opts),
	} {
		for _, want := range []string{
			fmt.Sprintf("reviewThreads(first:%d,after:$endCursor)", ReviewBotThreadPageSize),
			fmt.Sprintf("comments(first:%d)", ReviewBotThreadCommentsFetched),
			"--paginate",
			"Do not reply in, react to, or resolve these threads.",
			"Answer EVERY thread that command returns",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: kick missing %q", name, want)
			}
		}
		if strings.Contains(got, "comments(first:20)") {
			t.Errorf("%s: kick still fetches 20 comments per thread", name)
		}
	}
	if ReviewBotThreadCommentsFetched < 2 {
		t.Fatalf("ReviewBotThreadCommentsFetched=%d cannot show whether a finding was answered", ReviewBotThreadCommentsFetched)
	}
}
