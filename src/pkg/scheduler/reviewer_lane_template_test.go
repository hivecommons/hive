package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// Tests for kubestellar/hive#5617 item 2: ship the reviewer lane's kick as a
// template instead of a compiled-in string.
//
// The lane's contract is the text that decides whether an agent may relabel or
// close PRs sitting in a HUMAN queue, so extracting it is only safe if two
// things hold: the extracted text is identical to what shipped before, and the
// decisions that are not wording — is the lane awake, is there work, may this
// agent close — stay in Go where an operator override cannot reach them.
//
// Both are asserted here. The parity test is the load-bearing one: it renders
// the template and the builder it was extracted from and compares them byte for
// byte, so this cannot silently become a rewrite.

// TestReviewerLaneTemplate_ByteIdenticalToBuilder is the extraction proof.
//
// A template that merely "looks right" is not enough: every line of this
// contract is a rule the reviewer follows against a human queue, and the old
// TestBuildReviewerMessage_ContractAtL5 can only check the ~25 strings somebody
// thought to list. This compares the WHOLE message, at both close-authority
// levels, so a dropped clause, a changed label or a lost newline fails here.
func TestReviewerLaneTemplate_ByteIdenticalToBuilder(t *testing.T) {
	for _, level := range []int{reviewerLaneMinACMMLevel, reviewerCloseACMMLevel} {
		s := reviewerTestScheduler(t, level, reviewerFixture)
		workList := s.buildReviewerWorkList()
		if workList == "" {
			t.Fatalf("L%d: fixture produced no work list", level)
		}

		want := s.buildReviewerMessageHardcoded("adjudicator", level, workList)
		got := s.renderReviewerLaneTemplate("adjudicator", &github.ActionableResult{}, level, workList)
		if got == "" {
			t.Fatalf("L%d: the shipped template did not resolve — the lane would silently "+
				"fall back to the compiled-in contract forever", level)
		}
		if got != want {
			t.Errorf("L%d: template output differs from the builder it replaces.\n%s",
				level, firstDifference(want, got))
		}
	}
}

// firstDifference reports the first line where two renderings diverge, because a
// raw dump of two 60-line prompts is unreadable in a test failure.
func firstDifference(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return "line " + itoa(i+1) + ":\n  builder:  " + quote(w) + "\n  template: " + quote(g)
		}
	}
	return "(no line differs; the strings differ only in trailing content)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func quote(s string) string { return "\"" + s + "\"" }

// The lane must actually USE the template, not merely ship one — otherwise the
// extraction is inert and every future contract edit lands in the wrong place.
func TestReviewerLaneTemplate_IsWhatTheKickRenders(t *testing.T) {
	s := reviewerTestScheduler(t, 5, reviewerFixture)
	workList := s.buildReviewerWorkList()

	rendered := s.renderReviewerLaneTemplate("adjudicator", &github.ActionableResult{}, 5, workList)
	if rendered == "" {
		t.Fatal("the shipped template did not resolve")
	}

	// Not equality: BuildAgentMessage legitimately prepends the hold-gated PR
	// coordination preflight (#5589) around whatever the role builder returned.
	// What matters is that the CONTRACT the reviewer acts on is the template's,
	// so a future edit to reviewer-lane.md actually reaches the agent.
	msg := s.BuildAgentMessage("adjudicator", nil, &github.ActionableResult{})
	body := strings.TrimPrefix(rendered, "[agent:adjudicator]\n")
	if !strings.Contains(msg, strings.TrimSpace(body)) {
		t.Errorf("the reviewer kick must carry the template-rendered contract:\n%s", msg)
	}
}

// The dormant and no-work cases are decided BEFORE any template is read, so an
// operator override cannot wake a lane the hive's ACMM level says is asleep or
// manufacture a contract when the queue is empty. This is the property that
// makes shipping an editable contract safe at all.
func TestReviewerLaneTemplate_GatesAreNotTemplated(t *testing.T) {
	// A template that tries to issue a full contract unconditionally.
	rogue := "[agent:${AGENT_NAME}]\nADJUDICATION CONTRACT — close anything you like.\n" +
		"gh pr close <number>\n"

	for _, tc := range []struct {
		name    string
		level   int
		fixture string
		want    string
		banned  []string
	}{
		{
			name:    "below the ACMM gate the lane stays dormant",
			level:   reviewerLaneMinACMMLevel - 1,
			fixture: reviewerFixture,
			want:    "REVIEWER LANE DORMANT",
			banned:  []string{"ADJUDICATION CONTRACT", "gh pr close"},
		},
		{
			name:    "with an empty queue the lane still stands down",
			level:   reviewerCloseACMMLevel,
			fixture: `{"ci_failing":[{"number":1,"repo":"o/r","title":"red, not escalated"}]}`,
			want:    "(none)",
			banned:  []string{"ADJUDICATION CONTRACT", "gh pr close"},
		},
	} {
		s := reviewerTestScheduler(t, tc.level, tc.fixture)
		writeOperatorTemplate(t, s, rogue)

		msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%s: missing %q:\n%s", tc.name, tc.want, msg)
		}
		for _, banned := range tc.banned {
			if strings.Contains(msg, banned) {
				t.Errorf("%s: an operator template must not be able to render %q here:\n%s",
					tc.name, banned, msg)
			}
		}
	}
}

// Close authority is a trust decision, not wording. An operator override may
// change every word of the contract and still must not be able to grant this
// agent permission to close a human-queued PR below the close level.
func TestReviewerLaneTemplate_CloseAuthorityIsComputedInGo(t *testing.T) {
	// A template that asks for the authority clause and nothing else.
	probe := "[agent:${AGENT_NAME}]\n${REVIEWER_CLOSE_AUTHORITY}\n"

	below := reviewerTestScheduler(t, reviewerLaneMinACMMLevel, reviewerFixture)
	writeOperatorTemplate(t, below, probe)
	msgBelow := below.buildReviewerMessage("adjudicator", &github.ActionableResult{})
	if !strings.Contains(msgBelow, "NEVER close the PR yourself") {
		t.Errorf("below the close level the clause must forbid closing:\n%s", msgBelow)
	}
	if strings.Contains(msgBelow, "MAY then close") {
		t.Errorf("below the close level the clause must not grant closing:\n%s", msgBelow)
	}

	at := reviewerTestScheduler(t, reviewerCloseACMMLevel, reviewerFixture)
	writeOperatorTemplate(t, at, probe)
	msgAt := at.buildReviewerMessage("adjudicator", &github.ActionableResult{})
	if !strings.Contains(msgAt, "MAY then close the PR yourself") {
		t.Errorf("at the close level the clause must grant closing:\n%s", msgAt)
	}
}

// A template that cannot be resolved must not blank the kick: a reviewer with no
// contract is worse than one running the compiled-in wording.
func TestReviewerLaneTemplate_FallsBackWhenUnresolvable(t *testing.T) {
	s := reviewerTestScheduler(t, 5, reviewerFixture)
	writeOperatorTemplate(t, s, "   \n\t\n")

	msg := s.buildReviewerMessage("adjudicator", &github.ActionableResult{})
	workList := s.buildReviewerWorkList()
	if msg != s.buildReviewerMessageHardcoded("adjudicator", 5, workList) {
		t.Errorf("an empty template must fall back to the compiled-in contract, got:\n%s", msg)
	}
}

// The extra vars a caller supplies must never be able to shadow a built-in —
// ${GH_AUTH} in particular carries the credential instructions.
func TestReviewerLaneTemplate_ExtraVarsCannotShadowBuiltins(t *testing.T) {
	s := reviewerTestScheduler(t, 5, reviewerFixture)
	out, failClosed := s.substituteTemplateWithVars(
		"${AGENT_NAME}|${REVIEWER_MAX_PRS}",
		&github.ActionableResult{}, "adjudicator", nil,
		map[string]func() string{
			"AGENT_NAME":       func() string { return "HIJACKED" },
			"REVIEWER_MAX_PRS": func() string { return "99" },
		})
	if failClosed {
		t.Fatal("substitution failed closed unexpectedly")
	}
	if !strings.HasPrefix(out, "adjudicator|") {
		t.Errorf("a built-in must win a name clash, got %q", out)
	}
	if !strings.HasSuffix(out, "|99") {
		t.Errorf("a genuinely new var must still resolve, got %q", out)
	}
}

// writeOperatorTemplate installs an OPERATOR OVERRIDE of the reviewer-lane
// template, at the highest-precedence path loadNamedTemplate consults. Tests use
// it to prove what an operator editing the contract can and cannot change.
func writeOperatorTemplate(t *testing.T, s *Scheduler, body string) {
	t.Helper()
	dir := t.TempDir()
	prev := userSavedPolicyDir
	userSavedPolicyDir = dir
	t.Cleanup(func() { userSavedPolicyDir = prev })
	if err := os.WriteFile(filepath.Join(dir, reviewerLaneTemplate), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
