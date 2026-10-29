package delegatedecision

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every rule slug the policy can emit has a glossary entry, so no decision
// is ever shown to an operator with a bare slug (hivecommons/hive#14822:
// "what does supersede mean?"). The slugs are read off the source files
// beside this test rather than listed by hand, so a new rule without words
// fails here.
func TestGlossaryCoversEveryRuleSlug(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`Rule:\s*"([a-z0-9-]+)"`),
		regexp.MustCompile(`\b(?:recommendOnly|approveIssue|releasePR)\("([a-z0-9-]+)"`),
		regexp.MustCompile(`\bruleDefault[A-Za-z]+\s*=\s*"([a-z0-9-]+)"`),
		regexp.MustCompile(`\bRuleLLM\s*=\s*"([a-z0-9-]+)"`),
		regexp.MustCompile(`\bRuleReturnToAuthor\s*=\s*"([a-z0-9-]+)"`),
	}
	found := map[string]bool{}
	for _, file := range []string{"policy.go", "llm.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				found[m[1]] = true
			}
		}
	}
	if len(found) < 10 {
		t.Fatalf("rule slug scan found only %d slugs: %v", len(found), found)
	}
	for slug := range found {
		e, ok := Explain(slug)
		if !ok {
			t.Errorf("rule %q has no glossary entry", slug)
			continue
		}
		if e.Kind != GlossaryRule {
			t.Errorf("rule %q resolves to a %s entry", slug, e.Kind)
		}
		if strings.TrimSpace(e.Plain) == "" || strings.TrimSpace(e.Detail) == "" || strings.Contains(e.Plain, slug) {
			t.Errorf("rule %q: plain words must be non-empty and not repeat the slug: %+v", slug, e)
		}
	}
}

func TestGlossaryCoversVerdictsAndActions(t *testing.T) {
	for _, v := range []Verdict{VerdictApprove, VerdictDirection, VerdictCloseDuplicate, VerdictCloseSuperseded, VerdictOwnerOnly,
		VerdictQuarantineFlaky, VerdictAcceptRatchet, VerdictAcceptProposal, VerdictAdoptSupersede, VerdictRecommendOnly, VerdictRefused} {
		if _, ok := Explain(string(v)); !ok {
			t.Errorf("verdict %q has no glossary entry", v)
		}
	}
	for _, a := range AllowedActions {
		if _, ok := Explain(string(a)); !ok {
			t.Errorf("action %q has no glossary entry", a)
		}
	}
	if Plain(GlossarySupersede) != "close this PR and open a fresh hive-authored PR with the same commits" {
		t.Errorf("supersede plain words changed: %q", Plain(GlossarySupersede))
	}
	if Plain(string(VerdictAdoptSupersede)) != Plain(GlossarySupersede) || Plain(LabelAdoptSupersede) != Plain(GlossarySupersede) {
		t.Errorf("adopt-supersede aliases do not resolve to supersede")
	}
}

func TestGlossaryResolvesComposedLaneTokens(t *testing.T) {
	cases := map[string]string{
		"contributor-lane:changes-requested":                  GlossaryChangesRequest,
		"adoption-stalled:contributor-lane:changes-requested": GlossaryChangesRequest,
		"CHANGES_REQUESTED":                                   GlossaryChangesRequest,
		"sentinel-alert":                                      GlossarySentinel,
		"label:needs-human":                                   GlossaryNeedsHuman,
		"adoption/supersede":                                  GlossarySupersede,
		"adoption-stalled:" + GlossaryConflicting:             GlossaryConflicting,
	}
	for in, want := range cases {
		e, ok := Explain(in)
		if !ok || e.Slug != want {
			t.Errorf("Explain(%q) = %+v, %v; want slug %q", in, e, ok, want)
		}
	}
	// A blocker token resolves to its family (hivecommons/hive#15099).
	if e, ok := Explain("blocker:capability:workflows-permission"); !ok || e.Slug != GlossaryBlocker {
		t.Errorf("blocker family lookup = %+v, %v", e, ok)
	}
	if got := Plain("some-unknown_token"); got != "some unknown token" {
		t.Errorf("unknown slug is humanised, got %q", got)
	}
	if Plain("") != "" || RulePlain("  ") != "" {
		t.Error("empty slug renders as empty")
	}
}

func TestGlossaryJSONMirror(t *testing.T) {
	var m map[string]struct {
		Kind   string `json:"kind"`
		Plain  string `json:"plain"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(GlossaryJSON()), &m); err != nil {
		t.Fatal(err)
	}
	for _, e := range Glossary() {
		if m[e.Slug].Plain != e.Plain {
			t.Errorf("JSON mirror misses %q", e.Slug)
		}
		for _, a := range e.Aliases {
			if m[a].Plain != e.Plain {
				t.Errorf("JSON mirror misses alias %q of %q", a, e.Slug)
			}
		}
	}
}

func TestCommentBodyPlainWords(t *testing.T) {
	d := Decision{Verdict: VerdictAdoptSupersede, Rule: "contributor-offline", Text: "supersede now: adopt #14822 in supersede mode", Rationale: "offline",
		Actions: []Action{decisionComment("x"), {Kind: ActionAdoptSupersede}}}
	body := CommentBodyWith(d, "hive-a", "andy", CommentOptions{Kind: KindPR, EffectTimeout: 30 * time.Minute})
	for _, want := range []string{
		"/hive decision supersede now",
		"**Hive decided:** Close this PR and open a fresh hive-authored PR with the same commits.",
		"**Why:** The author hasn't been reachable since the nudge.",
		"**What happens next:** The hive closes this PR and opens a fresh hive-authored PR with the same commits and sign-off, then finishes that one — expected within 30 min.",
		"**If it doesn't happen:** A maintainer can merge this PR by hand, re-run the decision, close it as superseded, or send it back for a new commit.",
		MarkerPrefix,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, "/hive decision ") {
		t.Errorf("decision line must stay first: %q", body)
	}
	rec := Decision{Verdict: VerdictRecommendOnly, Rule: "sentinel-untrusted", Text: "hive recommends: review; owner to confirm", Rationale: "r", KeepsPark: true}
	body = CommentBody(rec, "h", "")
	if !strings.Contains(body, "**What happens next:** Nothing by itself.") || strings.Contains(body, "If it doesn't happen") {
		t.Errorf("park-keeping comment must say nothing happens by itself:\n%s", body)
	}
	if strings.Contains(PlainWords(d, CommentOptions{}), "contributor-offline") {
		t.Error("plain words must not show the rule slug bare")
	}
}

func TestStalledCommentBody(t *testing.T) {
	d := Decision{Verdict: VerdictAdoptSupersede, Rule: "contributor-offline"}
	at := time.Date(2026, 10, 28, 13, 1, 29, 0, time.UTC)
	body := StalledCommentBody(d, KindPR, at, "contributor-lane:changes-requested", []string{"Merge as maintainer", "Re-run decision"}, "hive-a")
	for _, want := range []string{
		"**Hive decided close this PR and open a fresh hive-authored PR with the same commits at 2026-10-28T13:01:29Z, but it hasn't taken effect.**",
		"**What should have happened:** The hive closes this PR",
		"**Why it hasn't:** A reviewer asked for changes on this branch.",
		"**What you can do:** Merge as maintainer · Re-run decision.",
		StalledMarkerPrefix + " by=hive-a rule=contributor-offline -->",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stalled comment lacks %q:\n%s", want, body)
		}
	}
	if !IsStalledComment(body, "contributor-offline") || IsStalledComment(body, "dedupe") || IsStalledComment("plain", "") {
		t.Error("IsStalledComment must match the rule stamped in the marker")
	}
	if IsDelegatedComment(body) {
		t.Error("the stalled follow-up must not read as a second decision")
	}
	if !strings.Contains(StalledCommentBody(d, KindPR, at, "", nil, "h"), Detail(GlossaryEffectUnknown)) {
		t.Error("unknown why falls back to the effect-unknown entry")
	}
}

// TestGlossaryWordsPerItemKind: an issue's `approve` is never "approved for
// merge" (hivecommons/hive#15021); entries without issue words fall back
// to their plain words; the JSON mirror carries the issue words.
func TestGlossaryWordsPerItemKind(t *testing.T) {
	approve := Decision{Verdict: VerdictApprove}
	if got := ActionPlainOn(approve, KindIssue); got != "approved to proceed — an agent may claim it" {
		t.Fatalf("issue approve = %q", got)
	}
	if got, want := ActionPlainOn(approve, KindPR), ActionPlain(approve); got != want || got != "approved for merge" {
		t.Fatalf("pr approve = %q / %q", got, want)
	}
	if got := ActionPlainOn(Decision{Verdict: VerdictDirection}, KindIssue); strings.Contains(got, "author agent") {
		t.Fatalf("issue direction = %q", got)
	}
	if got := ActionPlainOn(Decision{Verdict: VerdictCloseSuperseded}, KindIssue); strings.Contains(got, "PR") {
		t.Fatalf("issue close-superseded = %q", got)
	}
	if got, want := PlainOn(string(VerdictCloseDuplicate), KindIssue), Plain(string(VerdictCloseDuplicate)); got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}
	if got := DetailOn(GlossaryHold, KindIssue); !strings.Contains(got, "issue") || DetailOn(GlossaryHold, KindPR) == got {
		t.Fatalf("hold issue detail = %q", got)
	}
	if got := PlainOn("unknown-thing", KindIssue); got != "unknown thing" {
		t.Fatalf("unknown = %q", got)
	}
	words := PlainWords(approve, CommentOptions{Kind: KindIssue})
	if strings.Contains(words, "approved for merge") || !strings.Contains(words, "Approved to proceed") {
		t.Fatalf("issue PlainWords = %q", words)
	}
	stalled := StalledCommentBody(approve, KindIssue, time.Date(2026, 10, 28, 20, 41, 22, 0, time.UTC), GlossaryParkLabelStuck, nil, "h1")
	if strings.Contains(stalled, "approved for merge") || !strings.Contains(stalled, "stage/045-needs-decision park label was never removed") {
		t.Fatalf("stalled = %q", stalled)
	}
	var m map[string]map[string]string
	if err := json.Unmarshal([]byte(GlossaryJSON()), &m); err != nil {
		t.Fatal(err)
	}
	if m["approve"]["issue_plain"] != "approved to proceed — an agent may claim it" || m["approved"]["issue_plain"] != m["approve"]["issue_plain"] {
		t.Fatalf("mirror: %v", m["approve"])
	}
	if _, has := m["close-duplicate"]["issue_plain"]; has {
		t.Fatalf("mirror carries an empty issue_plain: %v", m["close-duplicate"])
	}
}
