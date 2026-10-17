package config

import (
	"strings"
	"testing"
)

// Exactly one origin/* label: a caller's label is kept and canonicalised,
// extras are dropped, and the source is only appended when no origin label is
// present. Other axes are never touched by the generic helper.
func TestWithOriginLabelKeepsExactlyOne(t *testing.T) {
	tests := []struct {
		name   string
		in     []string
		source string
		want   string
	}{
		{"agent lane source is a no-op", []string{"agent/scanner", "kind/bug"}, "scanner", "agent/scanner,kind/bug"},
		{"nil labels", nil, OriginSourceSelfReport, "origin/selfreport"},
		{"keeps caller origin", []string{"origin/dashboard", "agent/scanner"}, "scanner", "origin/dashboard,agent/scanner"},
		{"drops extra origins", []string{"origin/forge", "origin/convergence", "kind/bug"}, "governor", "origin/forge,kind/bug"},
		{"canonicalises legacy reviewer", []string{LegacyOriginReviewLabel, "agent/reviewer"}, "reviewer", "agent/reviewer"},
		{"maps legacy lane origins", []string{"origin/scanner", "kind/bug"}, "forge", "agent/scanner,kind/bug,origin/forge"},
		{"drops blanks", []string{" ", "kind/bug"}, "forge", "kind/bug,origin/forge"},
		{"blank source is a no-op", []string{"kind/bug"}, "", "kind/bug"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WithOriginLabel(tc.in, tc.source)
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("WithOriginLabel(%v, %q) = %v, want %s", tc.in, tc.source, got, tc.want)
			}
			origins := 0
			for _, l := range got {
				if IsOriginLabel(l) {
					origins++
				}
			}
			if origins > 1 {
				t.Fatalf("more than one origin label: %v", got)
			}
		})
	}
}

func TestWithIssueOriginLabelDropsRedundantAgentLaneAliases(t *testing.T) {
	tests := []struct {
		name   string
		in     []string
		source string
		want   string
	}{
		{"drops matching bare lane and ensures agent lane only", []string{"agent/outreach", "outreach", "kind/bug"}, "outreach", "agent/outreach,kind/bug"},
		{"keeps human topic words and workflow labels", []string{"community", "help wanted", "stage/020-triage"}, "outreach", "community,help wanted,stage/020-triage,agent/outreach"},
		{"replaces other known lane labels", []string{"agent/scanner", "scanner"}, "outreach", "agent/outreach"},
		{"keeps aliases for non-lane source", []string{"agent/scanner", "scanner"}, OriginSourceDashboard, "agent/scanner,scanner,origin/dashboard"},
		{"maps lane origin for non-lane source", []string{"origin/scanner", "kind/bug"}, OriginSourceForge, "agent/scanner,kind/bug,origin/forge"},
		{"keeps open ended lane labels", []string{"agent/new-lane", "new-lane"}, "new-lane", "agent/new-lane,new-lane,origin/new-lane"},
		{"drops origin lane and bare label", []string{"origin/scanner", "agent/scanner", "scanner"}, "scanner", "agent/scanner"},
		{"caller origin keeps non-lane provenance but bare filing lane dedupes", []string{"origin/dashboard", "agent/scanner", "scanner", "kind/bug"}, "scanner", "origin/dashboard,agent/scanner,kind/bug"},
		{"outreach filed issue gets agent outreach only", []string{"origin/outreach", "outreach"}, "outreach", "agent/outreach"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithIssueOriginLabel(tc.in, tc.source); strings.Join(got, ",") != tc.want {
				t.Fatalf("WithIssueOriginLabel(%v, %q) = %v, want %s", tc.in, tc.source, got, tc.want)
			}
		})
	}
}

func TestOriginLaneFromAgentStripsReplicaSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"scanner": "scanner", "scanner-3": "scanner", "ci-maintainer": "ci-maintainer", "ci-maintainer-12": "ci-maintainer",
		"Sec-Check": "sec-check", "": "", "   ": "", "not a lane": "",
	} {
		if got := OriginLaneFromAgent(in); got != want {
			t.Errorf("OriginLaneFromAgent(%q) = %q, want %q", in, got, want)
		}
	}
	if got := OriginLabelForAgent("quality-2"); got != "agent/quality" {
		t.Fatalf("OriginLabelForAgent = %q", got)
	}
	for in, want := range map[string]string{
		"scanner":        "agent/scanner",
		"scanner-2":      "agent/scanner",
		"agent/scanner":  "agent/scanner",
		"origin/scanner": "agent/scanner",
	} {
		if got, ok := CanonicalAgentLaneLabel(in); !ok || got != want {
			t.Fatalf("CanonicalAgentLaneLabel(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	if got, ok := CanonicalAgentLaneLabel("origin/forge"); ok || got != "" {
		t.Fatalf("CanonicalAgentLaneLabel(origin/forge) = %q, %v; want no lane", got, ok)
	}
}

// The reviewer source must resolve to the agent lane label the review-backlog
// sink and the rename migration use, and every origin source needs a
// description so the label-ensure path never creates an undocumented origin
// label.
func TestOriginVocabularyIsConsistent(t *testing.T) {
	if OriginLabel(OriginSourceReviewer) != "" || OriginLabelForAgent(OriginSourceReviewer) != DefaultReviewBacklogLabel {
		t.Fatalf("reviewer label drift: origin=%q agent=%q default=%q", OriginLabel(OriginSourceReviewer), OriginLabelForAgent(OriginSourceReviewer), DefaultReviewBacklogLabel)
	}
	sources := KnownOriginSources()
	if len(sources) == 0 {
		t.Fatal("no known origin sources")
	}
	seen := map[string]bool{}
	for _, s := range sources {
		if seen[s] {
			t.Fatalf("duplicate source %q", s)
		}
		seen[s] = true
		label := OriginLabel(s)
		if !IsOriginLabel(label) || OriginSourceOf(label) != s {
			t.Fatalf("round-trip failed for %q: %q", s, label)
		}
		if d, ok := OriginLabelDescription(label); !ok || d == "" || !strings.Contains(d, "provenance") {
			t.Fatalf("origin/%s description = %q", s, d)
		}
	}
	for _, must := range []string{"selfreport", "governor", "dashboard", "forge", "convergence"} {
		if !seen[must] {
			t.Errorf("source %q missing from KnownOriginSources", must)
		}
	}
	for _, lane := range []string{"reviewer", "scanner", "quality", "ci-maintainer", "sec-check", "architect", "guide", "strategist", "outreach", "dedupe", "supervisor", "tester"} {
		if seen[lane] {
			t.Errorf("agent lane %q must not be an origin source", lane)
		}
	}
	if seen["human"] {
		t.Error("origin/human must never be a hive-applied source")
	}
	if got, ok := FirstOriginLabel([]string{"kind/bug", "origin/forge", "origin/guide"}); !ok || got != "origin/forge" {
		t.Fatalf("FirstOriginLabel = %q %v", got, ok)
	}
}
