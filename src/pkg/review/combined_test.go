package review

import (
	"encoding/json"
	"strings"
	"testing"
)

func combinedReportJSON(t *testing.T, perspective, verdict, repo string, number int, head string) string {
	t.Helper()
	r := map[string]any{
		"lane": "review-swarm", "kind": "review", "findings": []any{}, "prs_opened": []any{}, "beads_filed": []any{},
		"summary": "ok", "perspective": perspective, "verdict": verdict, "repo": repo, "number": number, "head_sha": head,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A combined review delivers every perspective's verdict in one array, and the
// old one-object shape must keep working: every reviewer already deployed
// emits it, and the relay cannot tell which kick a verdict answers.
func TestValidateReportsAcceptsObjectAndArray(t *testing.T) {
	single := combinedReportJSON(t, "correctness", "approve", "o/r", 7, "abc")
	got, err := ValidateReports([]byte(single))
	if err != nil || len(got) != 1 || got[0].Perspective != PerspectiveCorrectness {
		t.Fatalf("single object: got %v err %v", got, err)
	}

	arr := "[" + strings.Join([]string{
		combinedReportJSON(t, "correctness", "approve", "o/r", 7, "abc"),
		combinedReportJSON(t, "security", "changes_requested", "o/r", 7, "abc"),
		combinedReportJSON(t, "docs-currency", "approve", "o/r", 7, ""),
	}, ",") + "]"
	got, err = ValidateReports([]byte(arr))
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	if len(got) != 3 || got[1].Verdict != VerdictChangesRequested {
		t.Fatalf("array: got %+v", got)
	}
}

// One array must judge one PR at one revision, and one perspective at most
// once. Each of these is a way a reviewer authorized for PR 7 could otherwise
// smuggle a verdict about something it was not asked about.
func TestValidateReportsRejectsInconsistentArrays(t *testing.T) {
	base := combinedReportJSON(t, "correctness", "approve", "o/r", 7, "abc")
	cases := map[string]struct {
		second string
		want   string
	}{
		"different PR":          {combinedReportJSON(t, "security", "approve", "o/r", 8, "abc"), "same PR"},
		"different repo":        {combinedReportJSON(t, "security", "approve", "o/other", 7, "abc"), "same PR"},
		"different head":        {combinedReportJSON(t, "security", "approve", "o/r", 7, "def"), "same head SHA"},
		"duplicate perspective": {combinedReportJSON(t, "correctness", "reject", "o/r", 7, "abc"), "more than once"},
		"bad element":           {strings.Replace(combinedReportJSON(t, "security", "approve", "o/r", 7, "abc"), `"approve"`, `"merge_it"`, 1), "report 1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateReports([]byte("[" + base + "," + tc.second + "]"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	for name, raw := range map[string]string{"empty array": "[]", "trailing junk": "[" + base + "] {}", "not json": "[nope"} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateReports([]byte(raw)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// A verdict may only claim a perspective this hive reviews with — including
// one the hive defined itself, and excluding a built-in it switched off.
func TestValidateReportsHonoursTheHivesPerspectiveSet(t *testing.T) {
	set, err := NewPerspectiveSet([]string{"correctness", "api-compat"}, map[string]string{"api-compat": "breaking changes to the public API"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateReportsFor([]byte(combinedReportJSON(t, "api-compat", "approve", "o/r", 7, "")), set); err != nil {
		t.Fatalf("hive-defined perspective rejected: %v", err)
	}
	if _, err := ValidateReportsFor([]byte(combinedReportJSON(t, "security", "approve", "o/r", 7, "")), set); err == nil {
		t.Fatal("a perspective this hive does not review with was accepted")
	}
	if _, err := ValidateReports([]byte(combinedReportJSON(t, "api-compat", "approve", "o/r", 7, ""))); err == nil {
		t.Fatal("a hive-defined perspective was accepted by a hive that never defined it")
	}
}

func TestPerspectiveSetResolution(t *testing.T) {
	t.Run("zero value is the default set", func(t *testing.T) {
		var s PerspectiveSet
		if s.Len() != len(DefaultPerspectives) || !s.Known(PerspectiveSecurity) {
			t.Fatalf("zero set = %v", s.List())
		}
		if s.Focus(PerspectiveCorrectness) != defaultFocus[PerspectiveCorrectness] {
			t.Fatal("zero set lost the built-in focus")
		}
	})
	t.Run("selection keeps operator order and dedups", func(t *testing.T) {
		s, err := NewPerspectiveSet([]string{" Security ", "correctness", "security"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Names(); got != "security, correctness" {
			t.Fatalf("names = %q", got)
		}
		if s.Known(PerspectiveStyle) {
			t.Fatal("a deselected built-in is still Known")
		}
	})
	t.Run("focus override wins and blank falls back", func(t *testing.T) {
		s, err := NewPerspectiveSet(nil, map[string]string{"style": "follow CONVENTIONS.md", "security": "   "})
		if err != nil {
			t.Fatal(err)
		}
		if s.Focus(PerspectiveStyle) != "follow CONVENTIONS.md" {
			t.Fatalf("override lost: %q", s.Focus(PerspectiveStyle))
		}
		if s.Focus(PerspectiveSecurity) != defaultFocus[PerspectiveSecurity] {
			t.Fatal("blank override did not fall back to the built-in")
		}
	})
	t.Run("custom perspective defined by focus alone joins the default set", func(t *testing.T) {
		s, err := NewPerspectiveSet(nil, map[string]string{"api-compat": "public API breakage"})
		if err != nil {
			t.Fatal(err)
		}
		if s.Len() != len(DefaultPerspectives)+1 || !s.Known("api-compat") {
			t.Fatalf("set = %v", s.List())
		}
	})
	t.Run("typo fails loudly", func(t *testing.T) {
		if _, err := NewPerspectiveSet([]string{"sekurity"}, nil); err == nil || !strings.Contains(err.Error(), "sekurity") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("custom name needs focus and a safe shape", func(t *testing.T) {
		if _, err := NewPerspectiveSet([]string{"api-compat"}, nil); err == nil {
			t.Fatal("custom perspective without focus accepted")
		}
		for _, bad := range []string{"../etc", "API Compat", "a--b", "-lead", strings.Repeat("x", MaxPerspectiveNameLen+1)} {
			if _, err := NewPerspectiveSet([]string{bad}, map[string]string{bad: "x"}); err == nil {
				t.Errorf("unsafe name %q accepted", bad)
			}
		}
	})
	t.Run("bounds", func(t *testing.T) {
		if _, err := NewPerspectiveSet(nil, map[string]string{"style": strings.Repeat("x", MaxFocusLen+1)}); err == nil {
			t.Fatal("oversized focus accepted")
		}
		names := map[string]string{}
		var sel []string
		for i := 0; i < MaxPerspectives+1; i++ {
			n := "p" + strings.Repeat("x", i+1)
			names[n], sel = "f", append(sel, n)
		}
		if _, err := NewPerspectiveSet(sel, names); err == nil {
			t.Fatal("too many perspectives accepted")
		}
	})
}

// A hive that reviews with three perspectives must reach unanimity on three.
// Holding it to the built-in five would make approve unreachable — the same
// trap the per-PR cap fell into.
func TestAggregateUnanimityUsesTheConfiguredSet(t *testing.T) {
	set, err := NewPerspectiveSet([]string{"correctness", "security", "docs-currency"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reports := []PerspectiveReport{approveReport(PerspectiveCorrectness), approveReport(PerspectiveSecurity), approveReport(PerspectiveDocsCurrency)}
	got := AggregateReports(reports, AggregateOptions{Perspectives: set})
	if got.Verdict != VerdictApprove || !got.MergeEligible {
		t.Fatalf("three of three did not approve: %+v", got)
	}
	if got := AggregateReports(reports[:2], AggregateOptions{Perspectives: set}); got.Verdict == VerdictApprove {
		t.Fatalf("two of three approved: %+v", got)
	}
	if got := AggregateReports(reports, AggregateOptions{}); got.Verdict == VerdictApprove {
		t.Fatalf("three of the default five approved: %+v", got)
	}
}
