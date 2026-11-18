package reviewrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func managedOnly(repos ...string) func(string) bool {
	return func(r string) bool {
		for _, m := range repos {
			if m == r {
				return true
			}
		}
		return false
	}
}

func replyBlock(rules string) string {
	return "Fixed in abc123.\n<!-- hive:review-rules {\"rules\":[" + rules + "]} -->\n"
}

func entry(text, scope, applies, url string) string {
	return `{"text":"` + text + `","scope":"` + scope + `","applies_to":"` + applies + `","evidence_url":"` + url + `"}`
}

func seeded(t *testing.T, assoc string) (*Store, string, string, Comment) {
	t.Helper()
	s, dir, vault := openStore(t, Config{})
	c := cmt("inline:1001", assoc, maintainersComment)
	if err := s.Remember(c, t0); err != nil {
		t.Fatal(err)
	}
	return s, dir, vault, c
}

func TestApplyReplyRefinesACapturedRule(t *testing.T) {
	s, dir, vault, c := seeded(t, "OWNER")
	s.CaptureComment(c, t0)
	body := replyBlock(entry("The maintainers file is the source of truth for ownership in every assessment.", "workflow", "cncf-techdocs-assessment", c.URL))
	got := s.ApplyReply("reply-1", body, managedOnly(), t0.Add(time.Hour))
	if len(got) != 1 || got[0].Outcome != OutcomeRefined {
		t.Fatalf("applied = %+v", got)
	}
	r := s.Get("R-1")
	if r.Scope != ScopeWorkflow || r.AppliesTo != "cncf-techdocs-assessment" || !strings.Contains(r.Text, "every assessment") || r.Confidence != confidenceAgent {
		t.Fatalf("rule = %+v", r)
	}
	if !strings.Contains(readFile(t, filepath.Join(vault, "rule-r-1.md")), "scope:\n  lanes: [cncf-techdocs-assessment]") {
		t.Fatal("the workflow scope must reach the entry")
	}
	// The same reply applies once, across a reload too.
	if again := s.ApplyReply("reply-1", body, nil, t0); again != nil {
		t.Fatalf("a reply is applied once: %+v", again)
	}
	reopened, _ := Open(dir, vault, Config{})
	if again := reopened.ApplyReply("reply-1", body, nil, t0); again != nil {
		t.Fatalf("a reply is applied once across a reload: %+v", again)
	}
}

func TestApplyReplyScopeDefaults(t *testing.T) {
	tests := []struct {
		name      string
		scope     string
		appliesTo string
		wantScope Scope
		wantApply string
	}{
		{"scope omitted is the authoring agent", "", "", ScopeAgent, "quality"},
		{"agent without a name", "agent", "", ScopeAgent, "quality"},
		{"agent with a name", "agent", "reviewer", ScopeAgent, "reviewer"},
		{"global drops the name", "global", "ignored", ScopeGlobal, ""},
		{"repo keeps the slug", "repo", "cncf/techdocs", ScopeRepo, "cncf/techdocs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, c := seeded(t, "OWNER")
			s.CaptureComment(c, t0)
			got := s.ApplyReply("r", replyBlock(entry("Use the maintainers file as the ownership source of truth.", tc.scope, tc.appliesTo, c.URL)), managedOnly("cncf/techdocs"), t0)
			if len(got) != 1 || got[0].Outcome != OutcomeRefined {
				t.Fatalf("applied = %+v", got)
			}
			if r := s.Get("R-1"); r.Scope != tc.wantScope || r.AppliesTo != tc.wantApply {
				t.Fatalf("scope=%s applies_to=%q, want %s %q", r.Scope, r.AppliesTo, tc.wantScope, tc.wantApply)
			}
		})
	}
}

func TestApplyReplyRejectsWhatItCannotTie(t *testing.T) {
	s, _, _, c := seeded(t, "OWNER")
	s.CaptureComment(c, t0)
	tests := []struct {
		name   string
		body   string
		reason string
	}{
		{"evidence the hive never saw", replyBlock(entry("t", "", "", "https://github.com/cncf/techdocs/pull/424#discussion_r999")), "the hive saw"},
		{"invalid entry", replyBlock(entry("", "", "", c.URL)), "text is empty"},
		{"not json", "x <!-- hive:review-rules {\"rules\": [1]} -->", "valid JSON"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.ApplyReply("rej-"+string(rune('a'+i)), tc.body, nil, t0)
			if len(got) == 0 || got[0].Outcome != OutcomeRejected || !strings.Contains(got[0].Reason, tc.reason) {
				t.Fatalf("applied = %+v, want a rejection mentioning %q", got, tc.reason)
			}
		})
	}
	if len(s.List()) != 1 || s.Get("R-1").Text != ".project/maintainers.yaml is the source of truth for ownership and maintainership" {
		t.Fatal("a rejected block must change nothing")
	}
	// A reply with no block is not recorded at all.
	if got := s.ApplyReply("plain", "Fixed, thanks.", nil, t0); got != nil || len(s.st.Replies) != 3 {
		t.Fatalf("plain reply: applied=%v replies=%d", got, len(s.st.Replies))
	}
}

func TestApplyReplyCapturesFromACommentTheClassifierMissed(t *testing.T) {
	s, _, vault := openStore(t, Config{})
	missed := cmt("inline:2002", "MEMBER", "Agent readiness is scored from the repo's AGENTS.md alone.")
	untrusted := cmt("inline:2003", "CONTRIBUTOR", "Weights come from the rubric page.")
	for _, c := range []Comment{missed, untrusted} {
		if err := s.Remember(c, t0); err != nil {
			t.Fatal(err)
		}
	}
	body := replyBlock(entry("Score agent readiness from the AGENTS.md file only.", "agent", "quality", missed.URL) + "," +
		entry("Take weights from the rubric page.", "", "", untrusted.URL))
	got := s.ApplyReply("r", body, nil, t0)
	if len(got) != 2 || got[0].Outcome != OutcomeCaptured || got[1].Outcome != OutcomeQueued {
		t.Fatalf("applied = %+v", got)
	}
	if r := got[0].Rule; r.Provenance[0].Source != "agent" || r.Confidence != confidenceAgentFromSeen || r.Status != StatusActive {
		t.Fatalf("rule = %+v", r)
	}
	if got[1].Rule.Status != StatusPending {
		t.Fatal("trust comes from the cited comment's association, never from the agent")
	}
	if !strings.Contains(readFile(t, filepath.Join(vault, "rule-r-2.md")), "state: draft") {
		t.Fatal("a queued agent rule is a draft entry")
	}
}

func TestApplyReplyForeignHome(t *testing.T) {
	s, _, _, c := seeded(t, "OWNER")
	s.CaptureComment(c, t0)
	body := replyBlock(entry("Projects list owners in their maintainers file.", "repo", "cncf/techdocs", c.URL))
	if got := s.ApplyReply("r1", body, managedOnly("hivecommons/hive"), t0); len(got) != 1 || got[0].Outcome != OutcomeRefined {
		t.Fatalf("applied = %+v", got)
	}
	props := s.HomeProposals()
	if len(props) != 1 || props[0].ForeignHome != "cncf/techdocs" || props[0].Scope != ScopeRepo {
		t.Fatalf("proposals = %+v", props)
	}
	if err := s.MarkHomeNoted("R-1"); err != nil {
		t.Fatal(err)
	}
	// The same destination does not re-open the proposal; a new one does.
	s.ApplyReply("r2", replyBlock(entry("Projects list owners in their maintainers file, always.", "repo", "cncf/techdocs", c.URL)), managedOnly(), t0)
	if len(s.HomeProposals()) != 0 {
		t.Fatal("an unchanged destination is not proposed twice")
	}
	s.ApplyReply("r3", replyBlock(entry("Projects list owners in their maintainers file, always.", "repo", "other/project", c.URL)), managedOnly(), t0)
	if p := s.HomeProposals(); len(p) != 1 || p[0].ForeignHome != "other/project" {
		t.Fatalf("a new destination is proposed once: %+v", p)
	}
	// A managed destination is the hive's own: no proposal.
	s2, _, _, c2 := seeded(t, "OWNER")
	s2.CaptureComment(c2, t0)
	s2.ApplyReply("r", replyBlock(entry("t", "repo", "hivecommons/hive", c2.URL)), managedOnly("hivecommons/hive"), t0)
	if len(s2.HomeProposals()) != 0 {
		t.Fatal("a managed repo needs no proposal")
	}
}

func TestApplyReplyMergesARefinedRuleIntoItsTwin(t *testing.T) {
	s, _, vault := openStore(t, Config{})
	c1 := cmt("inline:1", "OWNER", maintainersComment)
	c2 := cmt("inline:2", "OWNER", "Going forward, cite the scorecard with a commit-pinned link.")
	for _, c := range []Comment{c1, c2} {
		_ = s.Remember(c, t0)
		s.CaptureComment(c, t0)
	}
	got := s.ApplyReply("r", replyBlock(entry("The .project/maintainers.yaml is the source of truth for ownership and maintainership.", "", "", c2.URL)), nil, t0.Add(time.Hour))
	if len(got) != 1 || got[0].Outcome != OutcomeMerged || got[0].Rule.ID != "R-1" || len(got[0].Rule.Provenance) != 2 {
		t.Fatalf("applied = %+v", got)
	}
	r2 := s.Get("R-2")
	if r2.Status != StatusMerged || r2.MergedInto != "R-1" {
		t.Fatalf("R-2 = %+v", r2)
	}
	entry2 := readFile(t, filepath.Join(vault, "rule-r-2.md"))
	if !strings.Contains(entry2, "state: superseded\n") || !strings.Contains(entry2, "superseded_by: rule-r-1\n") {
		t.Fatalf("a merged rule's entry must be superseded by the survivor:\n%s", entry2)
	}
	if again := s.CaptureComment(c2, t0); again.Outcome != OutcomeDuplicate || again.Rule.ID != "R-1" {
		t.Fatalf("a merged rule's comment resolves to the survivor: %+v", again)
	}
	// Refining the older rule to the newer one's wording keeps the older id.
	s3, _, _ := openStore(t, Config{})
	for _, c := range []Comment{c1, c2} {
		_ = s3.Remember(c, t0)
		s3.CaptureComment(c, t0)
	}
	out := s3.ApplyReply("r", replyBlock(entry("Going forward, cite the scorecard with a commit-pinned link.", "", "", c1.URL)), nil, t0)
	if len(out) != 1 || out[0].Outcome != OutcomeMerged || out[0].Rule.ID != "R-1" || s3.Get("R-2").MergedInto != "R-1" {
		t.Fatalf("older id must survive: %+v", out)
	}
}

func TestApplyReplyRefinesOnlyLiveRules(t *testing.T) {
	s, _, _, c := seeded(t, "OWNER")
	s.CaptureComment(c, t0)
	if err := s.Retract("R-1", "operator", t0); err != nil {
		t.Fatal(err)
	}
	got := s.ApplyReply("r", replyBlock(entry("Use the maintainers file.", "", "", c.URL)), nil, t0)
	if len(got) != 1 || got[0].Outcome != OutcomeRejected || !strings.Contains(got[0].Reason, "no longer live") {
		t.Fatalf("applied = %+v", got)
	}
}

func TestApplyReplyRefinementOfAQueuedRuleKeepsItQueued(t *testing.T) {
	s, _, _, c := seeded(t, "CONTRIBUTOR")
	s.CaptureComment(c, t0)
	got := s.ApplyReply("r", replyBlock(entry("Use the maintainers file as the ownership source of truth.", "", "", c.URL)), nil, t0)
	if len(got) != 1 || got[0].Outcome != OutcomeRefined || got[0].Rule.Status != StatusPending || got[0].Rule.Confidence != confidencePending {
		t.Fatalf("applied = %+v", got)
	}
}

func TestMergeActivatesAQueuedSurvivor(t *testing.T) {
	s, _, _ := openStore(t, Config{})
	queued := cmt("inline:1", "CONTRIBUTOR", maintainersComment)
	trusted := cmt("inline:2", "OWNER", "Going forward, cite the scorecard with a commit-pinned link.")
	for _, c := range []Comment{queued, trusted} {
		_ = s.Remember(c, t0)
		s.CaptureComment(c, t0)
	}
	got := s.ApplyReply("r", replyBlock(entry("The .project/maintainers.yaml is the source of truth for ownership and maintainership.", "", "", trusted.URL)), nil, t0)
	if len(got) != 1 || got[0].Outcome != OutcomeMerged || got[0].Rule.Status != StatusActive {
		t.Fatalf("a trusted twin must activate the queued survivor: %+v", got)
	}
}

func TestApplyReplyPrunesRememberedReplies(t *testing.T) {
	s, _, _, c := seeded(t, "OWNER")
	s.CaptureComment(c, t0)
	for i := 0; i < maxRepliesKept+3; i++ {
		id := "reply-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i/26)
		s.ApplyReply(id, "<!-- hive:review-rules {} -->", nil, t0.Add(time.Duration(i)*time.Second))
	}
	if len(s.st.Replies) > maxRepliesKept {
		t.Fatalf("replies = %d, want at most %d", len(s.st.Replies), maxRepliesKept)
	}
}

func TestVaultEntryShapes(t *testing.T) {
	s, _, vault := openStore(t, Config{})
	repoRule := cmt("inline:1", "OWNER", "Going forward, list owners in the maintainers file.")
	s.CaptureComment(repoRule, t0)
	_ = s.Remember(repoRule, t0)
	s.ApplyReply("r", replyBlock(entry("List owners in the maintainers file.", "repo", "cncf/techdocs", repoRule.URL)), managedOnly("cncf/techdocs"), t0)
	b := readFile(t, filepath.Join(vault, "rule-r-1.md"))
	if strings.Contains(b, ", pin") || !strings.Contains(b, "repo:cncf/techdocs") || !strings.Contains(b, "scope:\n  lanes: [quality]") {
		t.Fatalf("a repo rule is not pinned, carries its repo tag and stays in its lane:\n%s", b)
	}
	global := cmt("inline:2", "OWNER", "From now on always run the linter before pushing.")
	_ = s.Remember(global, t0)
	s.CaptureComment(global, t0)
	s.ApplyReply("g", replyBlock(entry("Always run the linter before pushing.", "global", "", global.URL)), nil, t0)
	g := readFile(t, filepath.Join(vault, "rule-r-2.md"))
	if !strings.Contains(g, ", pin") || strings.Contains(g, "scope:") {
		t.Fatalf("a global rule is pinned and unscoped:\n%s", g)
	}
	if (&Rule{Scope: ScopeRepo}).lanes() != nil {
		t.Fatal("a repo rule without a lane has no lane scope")
	}
}

func TestDescribeAndHelpers(t *testing.T) {
	tests := []struct {
		r    Rule
		want string
	}{
		{Rule{Scope: ScopeAgent, AppliesTo: "quality"}, "the `quality` agent's future work"},
		{Rule{Scope: ScopeWorkflow, AppliesTo: "wf"}, "the `wf` workflow"},
		{Rule{Scope: ScopeRepo, AppliesTo: "o/r"}, "work on `o/r`"},
		{Rule{Scope: ScopeGlobal}, "all agents' work"},
	}
	for _, tc := range tests {
		if got := tc.r.Describe(); got != tc.want {
			t.Errorf("Describe = %q, want %q", got, tc.want)
		}
	}
	if got := tagValue("a b/c:d!"); got != "a-b/c:d" {
		t.Errorf("tagValue = %q", got)
	}
	if got := firstRunes("abcdef", 3); got != "abc..." {
		t.Errorf("firstRunes = %q", got)
	}
	if got := firstRunes("ab", 3); got != "ab" {
		t.Errorf("firstRunes = %q", got)
	}
	if got := errText(nil); got != "" {
		t.Errorf("errText = %q", got)
	}
}

func TestEntryWriteFailureIsReported(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.TempDir(), filepath.Join(blocker, "vault"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	res := s.CaptureComment(cmt("inline:1", "OWNER", maintainersComment), t0)
	if res.Outcome != OutcomeRefused || res.Err == nil || len(s.List()) != 0 {
		t.Fatalf("result = %+v, want a refusal when the vault cannot be written", res)
	}
}
