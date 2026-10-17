package config

import (
	"strings"
	"testing"
)

// Exactly one origin/* label per issue: a caller's label is kept and
// canonicalised, extras are dropped, and the source is only appended when no
// origin label is present. Other axes are never touched.
func TestWithOriginLabelKeepsExactlyOne(t *testing.T) {
	tests := []struct {
		name   string
		in     []string
		source string
		want   string
	}{
		{"appends when absent", []string{"agent/scanner", "kind/bug"}, "scanner", "agent/scanner,kind/bug,origin/scanner"},
		{"nil labels", nil, OriginSourceSelfReport, "origin/selfreport"},
		{"keeps caller origin", []string{"origin/dashboard", "agent/scanner"}, "scanner", "origin/dashboard,agent/scanner"},
		{"drops extra origins", []string{"origin/forge", "origin/convergence", "kind/bug"}, "governor", "origin/forge,kind/bug"},
		{"canonicalises legacy reviewer", []string{LegacyOriginReviewLabel, "agent/reviewer"}, "reviewer", OriginReviewerLabel + ",agent/reviewer"},
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

func TestOriginLaneFromAgentStripsReplicaSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"scanner": "scanner", "scanner-3": "scanner", "ci-maintainer": "ci-maintainer", "ci-maintainer-12": "ci-maintainer",
		"Sec-Check": "sec-check", "": "", "   ": "", "not a lane": "",
	} {
		if got := OriginLaneFromAgent(in); got != want {
			t.Errorf("OriginLaneFromAgent(%q) = %q, want %q", in, got, want)
		}
	}
	if got := OriginLabelForAgent("quality-2"); got != "origin/quality" {
		t.Fatalf("OriginLabelForAgent = %q", got)
	}
}

// The reviewer source must resolve to the same label the review-backlog sink
// and the rename migration use, and every known source needs a description
// so the label-ensure path never creates an undocumented origin label.
func TestOriginVocabularyIsConsistent(t *testing.T) {
	if OriginLabel(OriginSourceReviewer) != OriginReviewerLabel || OriginLabel(OriginSourceReviewer) != DefaultReviewBacklogLabel {
		t.Fatalf("reviewer label drift: %q %q %q", OriginLabel(OriginSourceReviewer), OriginReviewerLabel, DefaultReviewBacklogLabel)
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
	for _, must := range []string{"reviewer", "selfreport", "scanner", "quality", "ci-maintainer", "sec-check", "architect", "guide", "strategist", "outreach", "dedupe", "supervisor", "tester", "governor", "dashboard", "forge", "convergence"} {
		if !seen[must] {
			t.Errorf("source %q missing from KnownOriginSources", must)
		}
	}
	if seen["human"] {
		t.Error("origin/human must never be a hive-applied source")
	}
	if got, ok := FirstOriginLabel([]string{"kind/bug", "origin/forge", "origin/guide"}); !ok || got != "origin/forge" {
		t.Fatalf("FirstOriginLabel = %q %v", got, ok)
	}
}
