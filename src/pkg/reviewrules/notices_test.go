package reviewrules

import (
	"strings"
	"testing"
	"time"
)

func TestAckComment(t *testing.T) {
	r := &Rule{ID: "R-1", Text: "Ask @nate-double-u; see ``` fence", Scope: ScopeAgent, AppliesTo: "quality"}
	got := AckComment([]Ack{{Rule: r, URL: "https://github.com/cncf/techdocs/pull/424#discussion_r1"}})
	for _, want := range []string{AckMarkerPrefix + " ids=R-1 -->", "Recorded as rule R-1 in the hive's knowledge, applies to the `quality` agent's future work.",
		"Source: https://github.com/cncf/techdocs/pull/424#discussion_r1", "`@nate-double-u`", "'''", "hivectl knowledge state", "rule-r-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("ack missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "```text\nAsk @") || strings.Count(got, "```") != 2 {
		t.Errorf("rule text must sit in one fence it cannot close:\n%s", got)
	}
	if strings.Contains(got, "\u2014") {
		t.Error("no em-dashes")
	}
	merged := AckComment([]Ack{{Rule: r, URL: "u1", Merged: true}, {Rule: &Rule{ID: "R-2", Text: "t", Scope: ScopeGlobal}, URL: "u2"}})
	if !strings.Contains(merged, "This restates rule R-1") || !strings.Contains(merged, "ids=R-1,R-2") || !strings.Contains(merged, "Recorded as rule R-2") {
		t.Errorf("merged ack:\n%s", merged)
	}
}

func TestHomeProposalNote(t *testing.T) {
	r := &Rule{ID: "R-3", Text: ".project/maintainers.yaml is the source of truth for ownership", ForeignHome: "cncf/techdocs",
		Provenance: []Provenance{{CommentURL: "https://github.com/cncf/techdocs/pull/424#discussion_r1"}}}
	got := HomeProposalNote(r)
	for _, want := range []string{HomeMarkerPrefix + " rule=R-3 -->", "Needs a human", "`cncf/techdocs`", "does not write to that repo",
		"```text\n.project/maintainers.yaml is the source of truth for ownership\n```", "Source: https://github.com/cncf/techdocs/pull/424#discussion_r1"} {
		if !strings.Contains(got, want) {
			t.Errorf("note missing %q:\n%s", want, got)
		}
	}
	r.Provenance = nil
	if strings.Contains(HomeProposalNote(r), "Source:") {
		t.Error("no provenance, no source line")
	}
}

func TestNoticesAreOnePerRuleThroughTheStore(t *testing.T) {
	s, _, _ := openStore(t, Config{})
	c := cmt("inline:1", "OWNER", "Going forward, put the criteria in your docs: the maintainers file is the source of truth.")
	s.CaptureComment(c, t0)
	s.CaptureComment(c, t0.Add(time.Minute))
	if n := len(s.HomeProposals()); n != 1 {
		t.Fatalf("proposals = %d, want exactly one for the one rule", n)
	}
}
