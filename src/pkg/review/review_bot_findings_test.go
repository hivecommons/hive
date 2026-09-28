package review

import (
	"os/exec"
	"strings"
	"testing"
)

// hivecommons/hive#9360: a reviewer posted "Confidence: 5/5 (safe)" four
// minutes after Codex left a correct P1 security finding on the same commit,
// because nothing in its kick told it the bot's threads existed. Every kick —
// combined or per-perspective, whichever dispatch path the hive runs — must
// now fetch the configured bots' unresolved threads and hold a confirmed
// in-scope P0/P1 among them to the same bar as the reviewer's own finding.
func TestDispatchedKicksAnswerReviewBotFindings(t *testing.T) {
	prs := []PullRequest{{Repo: "Danathar/arch-bootc", Number: 410, HeadSHA: "sha1", Author: "hive-bot[bot]"}}
	base := DispatchOptions{
		RequireApproval: true, FanOut: true, MaxParallelReviews: 5,
		Agents:          []AgentCapability{reviewer("r1"), reviewer("r2")},
		AIAuthor:        "hive",
		ReviewBotLogins: []string{"chatgpt-codex-connector[bot]", "Copilot"},
	}
	for _, combined := range []bool{true, false} {
		o := base
		o.CombinedPerspectives = combined
		plan := PlanDispatch(prs, Artifact{}, DispatchState{}, o)
		if len(plan.ReviewKicks) == 0 {
			t.Fatalf("combined=%v: no kicks", combined)
		}
		for _, k := range plan.ReviewKicks {
			for _, want := range []string{
				"gh api graphql --paginate -f owner=Danathar -f name=arch-bootc -F number=410",
				`["chatgpt-codex-connector","copilot"] | index($a)`,
				"select(.isResolved | not)",
				"agree", "disagree", "cannot verify",
				"that perspective must not approve",
				"Never describe the PR as clean or safe while a confirmed in-scope P0/P1 bot finding stands",
				"it does not lower the verdict",
				"Do not reply in, react to, or resolve these threads",
			} {
				if !strings.Contains(k.Message, want) {
					t.Errorf("combined=%v perspective=%s: kick missing %q", combined, k.Perspective, want)
				}
			}
		}
	}
}

// A hive that has named no review bots gets no bot section: there is nothing
// to filter on, and telling the reviewer to answer threads it cannot find is
// an instruction it can only fail.
func TestNoReviewBotsConfiguredOmitsSection(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 1, HeadSHA: "sha"}
	for name, got := range map[string]string{
		"single":   BuildPerspectivePromptWith(PerspectiveCorrectness, pr, PromptOptions{ReviewBotLogins: []string{" ", ""}}),
		"combined": BuildCombinedPrompt(pr, nil, PromptOptions{}),
	} {
		if strings.Contains(got, "REVIEW-BOT FINDINGS") || strings.Contains(got, "reviewThreads") {
			t.Errorf("%s: bot section present with no configured bots", name)
		}
	}
}

// The logins are pasted into a shell command the reviewer runs, so a login
// GitHub could never issue must not reach it; and the GraphQL author login of
// an App lacks the "[bot]" suffix operators write in config, so the suffix is
// normalized away rather than silently matching nothing.
func TestReviewBotMatchLogins(t *testing.T) {
	got := reviewBotMatchLogins([]string{
		" chatgpt-codex-connector[bot] ", "CHATGPT-codex-connector", "Copilot",
		"x' ; rm -rf ~ ; '", `a"b`, "",
	})
	want := []string{"chatgpt-codex-connector", "copilot"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reviewBotMatchLogins = %v, want %v", got, want)
	}
}

// The prompt's jq filter is the whole selection logic, and a quoting slip in
// it would select nothing without any error. Run it against a GraphQL page
// shaped like GitHub's: an open Codex thread (GraphQL login, no [bot]), an
// open thread whose first comment is a human's, a resolved bot thread, and an
// open Copilot thread whose login differs in case from the config.
func TestReviewBotThreadFilterSelectsOpenBotThreads(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed")
	}
	section := buildReviewBotFindingsInstruction(PullRequest{Repo: "o/r", Number: 1}, []string{"chatgpt-codex-connector[bot]", "Copilot"})
	_, after, ok := strings.Cut(section, "--jq '")
	if !ok {
		t.Fatalf("no --jq filter in section:\n%s", section)
	}
	filter, _, ok := strings.Cut(after, "'\n")
	if !ok {
		t.Fatalf("unterminated --jq filter:\n%s", section)
	}
	page := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
{"isResolved":false,"isOutdated":false,"path":"scripts/quickstart.sh","line":12,"comments":{"nodes":[{"author":{"login":"chatgpt-codex-connector"},"body":"P1: signature checked after run","url":"u1"}]}},
{"isResolved":false,"isOutdated":false,"path":"a.go","line":3,"comments":{"nodes":[{"author":{"login":"alice"},"body":"human","url":"u2"},{"author":{"login":"chatgpt-codex-connector"},"body":"reply","url":"u3"}]}},
{"isResolved":true,"isOutdated":false,"path":"b.go","line":4,"comments":{"nodes":[{"author":{"login":"chatgpt-codex-connector"},"body":"fixed","url":"u4"}]}},
{"isResolved":false,"isOutdated":true,"path":"c.go","line":5,"comments":{"nodes":[{"author":{"login":"copilot"},"body":"nit","url":"u5"}]}}
]}}}}}`
	cmd := exec.Command(jq, "-c", filter)
	cmd.Stdin = strings.NewReader(page)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jq failed: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{`"path":"scripts/quickstart.sh"`, `"path":"c.go"`} {
		if !strings.Contains(got, want) {
			t.Errorf("filter dropped open bot thread %s:\n%s", want, got)
		}
	}
	for _, bad := range []string{`"path":"a.go"`, `"path":"b.go"`} {
		if strings.Contains(got, bad) {
			t.Errorf("filter selected %s (human-opened or resolved):\n%s", bad, got)
		}
	}
}
