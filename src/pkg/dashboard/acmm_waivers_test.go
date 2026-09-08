package dashboard

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The waiver mechanism exists because the ACMM criteria detect capability by
// looking for a file, and a capability can legitimately move off-repo. The
// concrete case: Danathar/sensi deleted .github/workflows/ai-fix.yml — a job
// holding contents/issues/pull-requests write plus an API key, gated on a
// label its own bot applied — and handed that work to hive. Two L4 criteria
// name that one filename, so the security fix cost the repo full green on a
// level it still met. Without waivers the only ways back are re-adding the
// file that was removed for cause, or loosening the criterion for every repo.
//
// The tests below pin the properties that keep a waiver from decaying into
// "criterion switched off": it must name a real criterion, it must carry a
// justification, it never overrides a detection, and it stays visibly marked.

// acmmWaiverContentJSON encodes a repo file the way the GitHub contents API
// does, which is what go-github's GetContent() decodes.
func acmmWaiverContentJSON(path, body string) map[string]interface{} {
	return map[string]interface{}{
		"name":     path,
		"path":     path,
		"type":     "file",
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString([]byte(body)),
	}
}

// acmmWaiverMux serves a repo whose root holds the named waiver files and
// whose .github/workflows is missing the AI-fix workflow. reqs counts every
// contents request so a test can assert calls that were NOT made.
func acmmWaiverMux(t *testing.T, files map[string]string, reqs *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/repos/myorg/repo1/contents/"):]
		if reqs != nil {
			reqs.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")

		if body, ok := files[path]; ok {
			_ = json.NewEncoder(w).Encode(acmmWaiverContentJSON(path, body))
			return
		}

		switch path {
		case "":
			entries := []map[string]interface{}{
				{"name": "README.md", "type": "file"},
				{"name": "go.mod", "type": "file"},
				{"name": "CLAUDE.md", "type": "file"},
				{"name": "CONTRIBUTING.md", "type": "file"},
			}
			for name := range files {
				entries = append(entries, map[string]interface{}{"name": name, "type": "file"})
			}
			_ = json.NewEncoder(w).Encode(entries)
		case ".github", ".github/workflows", ".github/ISSUE_TEMPLATE",
			".github/prompts", ".github/agents", ".claude", "docs", "docs/security":
			// Deliberately no ai-fix.yml / copilot-review-apply.yml here:
			// this is the post-deletion repo the waiver has to speak for.
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"name": "ci.yml", "type": "file"},
				{"name": "nightly.yml", "type": "file"},
			})
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func acmmWaiverServer(t *testing.T, ts *httptest.Server) *Server {
	t.Helper()
	s := NewServer(0, acmmEvalTestLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, acmmEvalTestLogger())
	s.RegisterAPI(deps)
	return s
}

const sensiWaiverYAML = `
waivers:
  - id: acmm:ai-fix-workflow
    satisfied_by: hive
    reason: >
      In-repo AI-fix path removed in PR #118; hive is the sole autonomous path.
  - id: acmm:copilot-review-apply
    satisfied_by: hive
    reason: Same; see docs/SECURITY-AI.md.
`

// ---------- parseACMMWaivers ----------

func TestParseACMMWaivers_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		body string
		want map[string]string // criterion ID -> expected satisfied_by
	}{
		{
			name: "two valid waivers are both honoured",
			body: sensiWaiverYAML,
			want: map[string]string{
				"acmm:ai-fix-workflow":      "hive",
				"acmm:copilot-review-apply": "hive",
			},
		},
		{
			name: "unknown criterion ID is dropped",
			// A typo must not sit in the file looking effective. If a
			// criterion is ever renamed, its waiver stops applying and the
			// gap re-opens as a visible red instead of staying green on a
			// stale ID nobody will re-read.
			body: "waivers:\n  - id: acmm:no-such-criterion\n    satisfied_by: hive\n    reason: typo\n",
			want: map[string]string{},
		},
		{
			name: "empty reason voids the waiver",
			body: "waivers:\n  - id: acmm:ai-fix-workflow\n    satisfied_by: hive\n    reason: \"\"\n",
			want: map[string]string{},
		},
		{
			name: "missing reason key voids the waiver",
			body: "waivers:\n  - id: acmm:ai-fix-workflow\n    satisfied_by: hive\n",
			want: map[string]string{},
		},
		{
			name: "missing satisfied_by voids the waiver",
			// Without it the declaration says only "do not check this",
			// which is the escape hatch this mechanism must not become.
			body: "waivers:\n  - id: acmm:ai-fix-workflow\n    reason: because\n",
			want: map[string]string{},
		},
		{
			name: "blank satisfied_by voids the waiver",
			body: "waivers:\n  - id: acmm:ai-fix-workflow\n    satisfied_by: \"   \"\n    reason: because\n",
			want: map[string]string{},
		},
		{
			name: "blank id is dropped",
			body: "waivers:\n  - id: \"\"\n    satisfied_by: hive\n    reason: nothing\n",
			want: map[string]string{},
		},
		{
			name: "duplicate id keeps the first declaration",
			body: "waivers:\n  - id: acmm:ai-fix-workflow\n    satisfied_by: hive\n    reason: first\n" +
				"  - id: acmm:ai-fix-workflow\n    satisfied_by: something-else\n    reason: second\n",
			want: map[string]string{"acmm:ai-fix-workflow": "hive"},
		},
		{
			name: "whitespace around fields is trimmed",
			body: "waivers:\n  - id: \"  acmm:ai-fix-workflow  \"\n    satisfied_by: \"  hive  \"\n    reason: \"  because  \"\n",
			want: map[string]string{"acmm:ai-fix-workflow": "hive"},
		},
		{name: "malformed YAML yields no waivers", body: "waivers: [", want: map[string]string{}},
		{name: "empty body yields no waivers", body: "", want: map[string]string{}},
		{name: "well-formed but empty waiver list", body: "waivers: []\n", want: map[string]string{}},
		{name: "unrelated YAML yields no waivers", body: "something_else: true\n", want: map[string]string{}},
		{
			name: "oversized file is refused rather than parsed",
			body: "waivers:\n  - id: acmm:ai-fix-workflow\n    satisfied_by: hive\n    reason: " +
				strings.Repeat("x", acmmMaxWaiverFileBytes) + "\n",
			want: map[string]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseACMMWaivers([]byte(tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d waivers, want %d: %#v", len(got), len(tc.want), got)
			}
			for id, satisfiedBy := range tc.want {
				w, ok := got[id]
				if !ok {
					t.Fatalf("waiver for %s missing", id)
				}
				if w.SatisfiedBy != satisfiedBy {
					t.Errorf("%s satisfied_by = %q, want %q", id, w.SatisfiedBy, satisfiedBy)
				}
				if w.Reason == "" {
					t.Errorf("%s kept with an empty reason", id)
				}
			}
		})
	}
}

// TestParseACMMWaivers_EveryIDMustExist walks the parser against the real
// criteria table so a criterion ID renamed in acmm_criteria.go cannot leave
// this feature accepting IDs that no longer mean anything.
func TestParseACMMWaivers_EveryIDMustExist(t *testing.T) {
	for _, c := range universalCriteria {
		body := "waivers:\n  - id: " + c.ID + "\n    satisfied_by: elsewhere\n    reason: covered\n"
		if got := parseACMMWaivers([]byte(body)); len(got) != 1 {
			t.Errorf("criterion %s cannot be waived; parser returned %d waivers", c.ID, len(got))
		}
	}
}

// ---------- fetchACMMWaivers ----------

func TestFetchACMMWaivers_ReadsRepoRootDeclaration(t *testing.T) {
	ts := acmmWaiverMux(t, map[string]string{".acmm.yml": sensiWaiverYAML}, nil)
	s := acmmWaiverServer(t, ts)

	cache := s.prefetchDirectories(t.Context(), "myorg", "repo1")
	got := s.fetchACMMWaivers(t.Context(), "myorg", "repo1", cache)

	if len(got) != 2 {
		t.Fatalf("got %d waivers, want 2: %#v", len(got), got)
	}
	if got["acmm:ai-fix-workflow"].SatisfiedBy != "hive" {
		t.Errorf("ai-fix waiver satisfied_by = %q, want hive", got["acmm:ai-fix-workflow"].SatisfiedBy)
	}
}

func TestFetchACMMWaivers_AcceptsYAMLSpelling(t *testing.T) {
	ts := acmmWaiverMux(t, map[string]string{".acmm.yaml": sensiWaiverYAML}, nil)
	s := acmmWaiverServer(t, ts)

	cache := s.prefetchDirectories(t.Context(), "myorg", "repo1")
	if got := s.fetchACMMWaivers(t.Context(), "myorg", "repo1", cache); len(got) != 2 {
		t.Fatalf(".acmm.yaml not honoured: got %d waivers", len(got))
	}
}

// TestFetchACMMWaivers_CostsNothingWhenRootLacksTheFile pins the budget
// property. A full refresh is already ~29 GetContents calls per repo inside a
// 20s per-repo timeout; charging every repo two more calls to discover a file
// almost none of them have would make waivers a fleet-wide tax.
func TestFetchACMMWaivers_CostsNothingWhenRootLacksTheFile(t *testing.T) {
	var reqs atomic.Int64
	ts := acmmWaiverMux(t, nil, &reqs)
	s := acmmWaiverServer(t, ts)

	cache := s.prefetchDirectories(t.Context(), "myorg", "repo1")
	before := reqs.Load()

	if got := s.fetchACMMWaivers(t.Context(), "myorg", "repo1", cache); got != nil {
		t.Fatalf("expected no waivers, got %#v", got)
	}
	if after := reqs.Load(); after != before {
		t.Errorf("fetchACMMWaivers made %d GitHub call(s) for a repo whose cached root listing has no .acmm.yml", after-before)
	}
}

// TestFetchACMMWaivers_QueriesWhenRootListingIsMissing covers the other half:
// when prefetch failed there is no cached root to rule the file out, and
// assuming "no waivers" would silently drop them on exactly the runs where
// GitHub was flaky.
func TestFetchACMMWaivers_QueriesWhenRootListingIsMissing(t *testing.T) {
	ts := acmmWaiverMux(t, map[string]string{".acmm.yml": sensiWaiverYAML}, nil)
	s := acmmWaiverServer(t, ts)

	if got := s.fetchACMMWaivers(t.Context(), "myorg", "repo1", map[string]map[string]bool{}); len(got) != 2 {
		t.Fatalf("expected waivers to be fetched on an empty cache, got %d", len(got))
	}
}

func TestFetchACMMWaivers_NilGHClient(t *testing.T) {
	s := NewServer(0, acmmEvalTestLogger())
	deps := testDeps(t)
	deps.GHClient = nil
	s.RegisterAPI(deps)

	if got := s.fetchACMMWaivers(t.Context(), "myorg", "repo1", nil); got != nil {
		t.Fatalf("expected nil waivers with no GitHub client, got %#v", got)
	}
}

// ---------- end-to-end through evaluateAllRepos ----------

// acmmResultFor finds one criterion in the single repo's results.
func acmmResultFor(t *testing.T, eval ACMMEvaluation, id string) CriterionResult {
	t.Helper()
	if len(eval.RepoResults) == 0 {
		t.Fatalf("evaluation produced no repo results (error: %q)", eval.Error)
	}
	for _, c := range eval.RepoResults[0].CriteriaResults {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("criterion %s absent from repo results", id)
	return CriterionResult{}
}

func acmmLevelFor(t *testing.T, eval ACMMEvaluation, level int) ACMMLevelScore {
	t.Helper()
	for _, l := range eval.RepoResults[0].Levels {
		if l.Level == level {
			return l
		}
	}
	t.Fatalf("level %d absent from repo results", level)
	return ACMMLevelScore{}
}

// TestEvaluateAllRepos_WaiverRestoresFullGreen is the sensi case end to end:
// the workflow file is gone, the repo declares why, and L4 reads 9/9 again
// with both rows marked rather than silently counted.
func TestEvaluateAllRepos_WaiverRestoresFullGreen(t *testing.T) {
	withWaiver := acmmWaiverServer(t, acmmWaiverMux(t, map[string]string{".acmm.yml": sensiWaiverYAML}, nil))
	without := acmmWaiverServer(t, acmmWaiverMux(t, nil, nil))

	base := without.evaluateAllRepos()
	baseL4 := acmmLevelFor(t, base, 4)

	eval := withWaiver.evaluateAllRepos()
	gotL4 := acmmLevelFor(t, eval, 4)

	if gotL4.Matched != baseL4.Matched+2 {
		t.Fatalf("L4 matched %d/%d with waivers, want %d (was %d without)",
			gotL4.Matched, gotL4.Total, baseL4.Matched+2, baseL4.Matched)
	}

	for _, id := range []string{"acmm:ai-fix-workflow", "acmm:copilot-review-apply"} {
		c := acmmResultFor(t, eval, id)
		if !c.Passed {
			t.Errorf("%s did not pass despite a waiver", id)
		}
		if !c.Waived {
			t.Errorf("%s passed without being marked waived — the panel would present a waiver as a detection", id)
		}
		if c.WaiverSatisfiedBy != "hive" {
			t.Errorf("%s waiver_satisfied_by = %q, want hive", id, c.WaiverSatisfiedBy)
		}
		if c.WaiverReason == "" {
			t.Errorf("%s is waived with no reason surfaced to the reader", id)
		}
	}
}

// TestEvaluateAllRepos_WaiverNeverMasksDetection: a criterion the repo really
// satisfies must not be reported as waived just because a stale declaration
// still names it. Otherwise a repo that fixed the gap keeps looking like it
// took the exemption, and removing the dead waiver looks risky when it is free.
func TestEvaluateAllRepos_WaiverNeverMasksDetection(t *testing.T) {
	// nightly.yml IS present in the fixture, so acmm:nightly-compliance
	// detects normally — while the waiver file also claims it.
	body := "waivers:\n  - id: acmm:nightly-compliance\n    satisfied_by: hive\n    reason: stale declaration\n"
	s := acmmWaiverServer(t, acmmWaiverMux(t, map[string]string{".acmm.yml": body}, nil))

	c := acmmResultFor(t, s.evaluateAllRepos(), "acmm:nightly-compliance")
	if !c.Passed {
		t.Fatal("acmm:nightly-compliance should have been detected in the fixture")
	}
	if c.Waived {
		t.Error("a detected criterion was reported as waived")
	}
	if c.WaiverReason != "" || c.WaiverSatisfiedBy != "" {
		t.Errorf("detection leaked waiver metadata: satisfied_by=%q reason=%q", c.WaiverSatisfiedBy, c.WaiverReason)
	}
}

// TestEvaluateAllRepos_UnwaivedGapStaysRed guards the obvious regression: the
// feature must not turn every miss green.
func TestEvaluateAllRepos_UnwaivedGapStaysRed(t *testing.T) {
	body := "waivers:\n  - id: acmm:ai-fix-workflow\n    satisfied_by: hive\n    reason: covered\n"
	s := acmmWaiverServer(t, acmmWaiverMux(t, map[string]string{".acmm.yml": body}, nil))

	eval := s.evaluateAllRepos()
	if c := acmmResultFor(t, eval, "acmm:ai-fix-workflow"); !c.Passed || !c.Waived {
		t.Fatalf("declared waiver not applied: passed=%v waived=%v", c.Passed, c.Waived)
	}
	// Declared for one criterion only; its neighbour naming the same missing
	// file must stay red.
	if c := acmmResultFor(t, eval, "acmm:copilot-review-apply"); c.Passed {
		t.Error("acmm:copilot-review-apply passed without a waiver of its own")
	}
}

// TestEvaluateAllRepos_AggregateMarksWaiverOnly checks the fleet-wide row.
// With one repo configured and that repo waiving, the aggregate is passed but
// waived — it must not claim the fleet detected something nobody has.
func TestEvaluateAllRepos_AggregateMarksWaiverOnly(t *testing.T) {
	s := acmmWaiverServer(t, acmmWaiverMux(t, map[string]string{".acmm.yml": sensiWaiverYAML}, nil))

	eval := s.evaluateAllRepos()
	var found bool
	for _, c := range eval.CriteriaResults {
		if c.ID != "acmm:ai-fix-workflow" {
			continue
		}
		found = true
		if !c.Passed {
			t.Error("aggregate row did not pass despite a repo waiver")
		}
		if !c.Waived {
			t.Error("aggregate row not marked waived when no repo detected the file")
		}
		if c.WaiverRepo != "repo1" {
			t.Errorf("aggregate waiver_repo = %q, want repo1", c.WaiverRepo)
		}
	}
	if !found {
		t.Fatal("acmm:ai-fix-workflow missing from aggregate results")
	}
}

// ---------- the guardrail: waivers restore green, never advance a level ----------

// acmmSyntheticLevel builds one level's worth of results: `detected` criteria
// met by detection, `waived` met by declaration, the rest failing.
func acmmSyntheticLevel(level, detected, waived, failing int) []CriterionResult {
	var out []CriterionResult
	add := func(n int, passed, isWaived bool, prefix string) {
		for i := 0; i < n; i++ {
			out = append(out, CriterionResult{
				ID:     prefix + string(rune('a'+i)),
				Level:  level,
				Passed: passed,
				Waived: isWaived,
			})
		}
	}
	add(detected, true, false, "det-")
	add(waived, true, true, "wav-")
	add(failing, false, false, "fail-")
	return out
}

// TestScoreResults_WaiversNeverAdvanceALevel is the load-bearing test for the
// whole mechanism's legitimacy.
//
// The waiver exists so a repo that moved a capability off-repo is not punished
// by a file-existence check — NOT so a repo can declare its way to a maturity
// level it has not reached. Level pass/fail is therefore computed on detected
// criteria alone. A repo that waives everything shows a full ratio and still
// does not pass, which is the difference between "the dashboard understands
// where my capability lives" and "the dashboard can be told anything".
func TestScoreResults_WaiversNeverAdvanceALevel(t *testing.T) {
	s := NewServer(0, acmmEvalTestLogger())

	tests := []struct {
		name                               string
		detected, waived, failing          int
		wantPassed                         bool
		wantMatched, wantDetected, wantTot int
	}{
		{
			name: "sensi's case: already over the bar, waivers restore full green",
			// 7 detected of 9 is 0.78, over 0.70 on its own merits. The two
			// waivers take the display to 9/9 without doing any of the work.
			detected: 7, waived: 2, failing: 0,
			wantPassed: true, wantMatched: 9, wantDetected: 7, wantTot: 9,
		},
		{
			name:     "waiving the whole level does not pass it",
			detected: 0, waived: 9, failing: 0,
			wantPassed: false, wantMatched: 9, wantDetected: 0, wantTot: 9,
		},
		{
			name: "waivers cannot lift a level from just under the bar",
			// 6/9 = 0.67, under 0.70. Three waivers would show 100%.
			detected: 6, waived: 3, failing: 0,
			wantPassed: false, wantMatched: 9, wantDetected: 6, wantTot: 9,
		},
		{
			name: "exactly at the threshold on detection alone still passes",
			// 7/10 = 0.70, the boundary. Detection carries it, not the waiver.
			detected: 7, waived: 1, failing: 2,
			wantPassed: true, wantMatched: 8, wantDetected: 7, wantTot: 10,
		},
		{
			name:     "no waivers behaves exactly as before",
			detected: 8, waived: 0, failing: 1,
			wantPassed: true, wantMatched: 8, wantDetected: 8, wantTot: 9,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eval := s.scoreResults(acmmSyntheticLevel(4, tc.detected, tc.waived, tc.failing))
			if len(eval.Levels) != 1 {
				t.Fatalf("expected one level, got %d", len(eval.Levels))
			}
			got := eval.Levels[0]
			if got.Passed != tc.wantPassed {
				t.Errorf("passed = %v, want %v (detected %d/%d = %.2f, displayed %d/%d)",
					got.Passed, tc.wantPassed, got.Detected, got.Total, got.DetectedScore, got.Matched, got.Total)
			}
			if got.Matched != tc.wantMatched || got.Total != tc.wantTot {
				t.Errorf("displayed ratio = %d/%d, want %d/%d", got.Matched, got.Total, tc.wantMatched, tc.wantTot)
			}
			if got.Detected != tc.wantDetected {
				t.Errorf("detected = %d, want %d", got.Detected, tc.wantDetected)
			}
			if got.Waived != tc.waived {
				t.Errorf("waived = %d, want %d", got.Waived, tc.waived)
			}
		})
	}
}

// TestScoreResults_WaiversDoNotRaiseCodebaseLevel is the same guarantee stated
// at the altitude an operator reads: the headline number. A repo cannot commit
// YAML and watch its ACMM level go up.
func TestScoreResults_WaiversDoNotRaiseCodebaseLevel(t *testing.T) {
	s := NewServer(0, acmmEvalTestLogger())

	// L0 and L2 genuinely met; L3 met only by waiver.
	var results []CriterionResult
	results = append(results, acmmSyntheticLevel(0, 8, 0, 0)...)
	results = append(results, acmmSyntheticLevel(2, 8, 0, 0)...)
	results = append(results, acmmSyntheticLevel(3, 0, 8, 0)...)

	eval := s.scoreResults(results)
	if eval.CodebaseLevel != 2 {
		t.Errorf("codebase level = %d, want 2 — a fully waived L3 advanced the repo", eval.CodebaseLevel)
	}

	// And the displayed L3 ratio is still full, so the operator sees 8/8 with
	// a ✗ and the asterisk rather than a silently zeroed level.
	for _, l := range eval.Levels {
		if l.Level != 3 {
			continue
		}
		if l.Matched != 8 || l.Passed {
			t.Errorf("L3 = %d/%d passed=%v, want 8/8 passed=false", l.Matched, l.Total, l.Passed)
		}
	}
}

// TestEvaluateAllRepos_WaiverSpreeStaysBlocked drives the same guarantee
// through the real evaluator, not just the scorer: a repo whose .acmm.yml
// waives every L4 criterion is still not L4.
func TestEvaluateAllRepos_WaiverSpreeStaysBlocked(t *testing.T) {
	var body strings.Builder
	body.WriteString("waivers:\n")
	for _, c := range universalCriteria {
		if c.Level == 4 {
			body.WriteString("  - id: " + c.ID + "\n    satisfied_by: hive\n    reason: all of it, honest\n")
		}
	}
	s := acmmWaiverServer(t, acmmWaiverMux(t, map[string]string{".acmm.yml": body.String()}, nil))

	eval := s.evaluateAllRepos()
	l4 := acmmLevelFor(t, eval, 4)

	if l4.Matched != l4.Total {
		t.Fatalf("expected every L4 criterion to be waived: got %d/%d", l4.Matched, l4.Total)
	}
	if l4.Passed {
		t.Error("a repo waived its way to a passing L4 — waivers must never advance a level")
	}
	if eval.RepoResults[0].CodebaseLevel >= 4 {
		t.Errorf("codebase level reached %d on waivers alone", eval.RepoResults[0].CodebaseLevel)
	}
}
