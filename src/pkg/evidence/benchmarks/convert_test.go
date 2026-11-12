package benchmarks

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

func t0(d time.Duration) time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC).Add(d) }

func fixtureBundles() []*evidence.Bundle {
	first := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: "a", Repo: "hivecommons/hive", Number: 42,
		Author:  evidence.Author{Login: "hive[bot]", Kind: evidence.AuthorAgent, Agent: "docs-writer", Backend: "copilot", Model: "claude-fable-5.1"},
		HeadSHA: "h1", Policy: evidence.Policy{ACMMLevel: 3},
		CI:        evidence.CI{Status: "failure"},
		CreatedAt: t0(0), UpdatedAt: t0(time.Hour),
	}
	latest := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: "b", Repo: "hivecommons/hive", Number: 42,
		Author: first.Author, HeadSHA: "h2", Policy: evidence.Policy{ACMMLevel: 3},
		CI: evidence.CI{Status: "success"},
		Verdicts: []evidence.Verdict{
			{Perspective: "security", Model: "claude-fable-5.1", Backend: "copilot", Verdict: "approve", Confidence: 0.9},
			{Perspective: "style", Model: "claude-fable-5.1", Backend: "copilot", Verdict: "comment", Confidence: 0.7},
		},
		HumanActions:  []evidence.Action{{Actor: "andy", Kind: "label", At: t0(2 * time.Hour)}},
		IssueClosures: []evidence.IssueClosure{{Repo: "hivecommons/hive", Issue: 40, At: t0(3 * time.Hour)}},
		Merge:         &evidence.MergeEvent{Actor: "hive", Method: "squash", At: t0(3 * time.Hour), OpenedAt: t0(-time.Hour)},
		CreatedAt:     t0(2 * time.Hour), UpdatedAt: t0(3 * time.Hour),
	}
	private := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: "c", Repo: "acme/secret", Number: 7,
		Author:  evidence.Author{Login: "x", Kind: evidence.AuthorAgent, Agent: "fixer", Backend: "pi", Model: "gpt-5.3-codex", Runtime: "podman", TaskKind: "issue-fix"},
		HeadSHA: "p1", Policy: evidence.Policy{ACMMLevel: 2},
		Outcome:   &evidence.Outcome{Source: "ledger", State: evidence.BackfillStateClosed, At: t0(5 * time.Hour)},
		CreatedAt: t0(0), UpdatedAt: t0(5 * time.Hour),
	}
	human := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: "d", Repo: "hivecommons/hive", Number: 43,
		Author:  evidence.Author{Login: "someone", Kind: evidence.AuthorHuman},
		HeadSHA: "q1", CreatedAt: t0(0), UpdatedAt: t0(time.Hour),
	}
	return []*evidence.Bundle{first, latest, private, human}
}

func testOptions() Options {
	return Options{
		HiveID: "hosted-test", HiveVersion: "v5.1.0", Now: t0(6 * time.Hour),
		RepoConsent:    true,
		RepoPublic:     func(repo string) bool { return strings.HasPrefix(repo, "hivecommons/") },
		SandboxRuntime: func(agent string) string { return "none" },
		CostLookup:     fixtureCostLookup,
	}
}

// fixtureCostLookup mimics the launch-cost store: the governor kicked short
// "hive#40" (issue the PR closed), the swarm reviewed full "hivecommons/hive#42".
func fixtureCostLookup(refs []CostRef) (Cost, bool) {
	spend := map[CostRef]float64{
		{Ref: "hive#40", Kind: "issue"}:          1.25,
		{Ref: "hivecommons/hive#42", Kind: "pr"}: 0.40,
		{Ref: "acme/secret#7", Kind: "issue"}:    0, // kick landed no usage
	}
	var usd float64
	n := 0
	for _, r := range refs {
		if v, ok := spend[r]; ok {
			usd += v
			n++
		}
	}
	if n == 0 {
		return Cost{Basis: CostBasisUnknown}, false
	}
	return Cost{Basis: CostBasisTokensXPricing, USDEst: &usd, Launches: n, PriceTableDate: "2026-11-09"}, true
}

func TestConvertAttributesCost(t *testing.T) {
	recs := Convert(fixtureBundles(), testOptions())
	author := byRole(recs, "hivecommons/hive", RoleAuthor)
	if author == nil || author.Cost.Basis != CostBasisTokensXPricing || author.Cost.USDEst == nil || *author.Cost.USDEst != 1.25 {
		t.Fatalf("author cost via closed issue short ref: %+v", author)
	}
	reviewer := byRole(recs, "hivecommons/hive", RoleReviewer)
	if reviewer == nil || reviewer.Cost.USDEst == nil || *reviewer.Cost.USDEst != 0.40 || reviewer.Cost.Launches != 1 {
		t.Fatalf("reviewer cost via full pr ref: %+v", reviewer)
	}
	private := byRole(recs, "", RoleAuthor)
	if private == nil || private.Cost.Basis != CostBasisUnknown || private.Cost.USDEst != nil {
		t.Fatalf("zero-usage kick must be unknown: %+v", private)
	}
	noLookup := testOptions()
	noLookup.CostLookup = nil
	for _, r := range Convert(fixtureBundles(), noLookup) {
		if r.Cost.Basis != CostBasisUnknown {
			t.Fatalf("nil lookup must yield unknown, got %+v", r.Cost)
		}
	}
}

func TestCostRefsSpellBothForms(t *testing.T) {
	b := &evidence.Bundle{Repo: "o/r", Number: 5, IssueClosures: []evidence.IssueClosure{{Issue: 3}}}
	got := authorCostRefs(b)
	want := []CostRef{{"o/r#5", "issue"}, {"r#5", "issue"}, {"o/r#3", "issue"}, {"r#3", "issue"}}
	if len(got) != len(want) {
		t.Fatalf("refs %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ref %d = %v want %v", i, got[i], want[i])
		}
	}
}

func byRole(recs []Record, repo string, role string) *Record {
	for i := range recs {
		if recs[i].Agent.Role == role && (recs[i].Task.Repo == repo || (repo == "" && recs[i].Task.Repo == "")) {
			return &recs[i]
		}
	}
	return nil
}

func TestConvertFoldsPRAndRedacts(t *testing.T) {
	recs := Convert(fixtureBundles(), testOptions())
	if len(recs) != 3 {
		t.Fatalf("want 3 records (author+reviewer for #42, author for private #7), got %d", len(recs))
	}
	a := byRole(recs, "hivecommons/hive", RoleAuthor)
	if a == nil {
		t.Fatal("missing author record for hivecommons/hive#42")
	}
	if a.Source.HiveID != HiveHash("hosted-test") || a.Source.Org != "hivecommons" || !a.Task.RepoConsent {
		t.Errorf("source/task attribution wrong: %+v %+v", a.Source, a.Task)
	}
	if a.Task.Kind != "docs" || a.Agent.Harness != "copilot" || a.Model.Provider != "anthropic" {
		t.Errorf("classification wrong: kind=%s harness=%s provider=%s", a.Task.Kind, a.Agent.Harness, a.Model.Provider)
	}
	o := a.Outcome
	if !o.PRMerged || o.MergeMethod != "squash" || o.Retries != 1 || o.HumanInterventions != 1 {
		t.Errorf("outcome wrong: %+v", o)
	}
	if o.CIGreenFirstTry == nil || *o.CIGreenFirstTry {
		t.Errorf("first head failed CI, want ci_green_first_try=false, got %v", o.CIGreenFirstTry)
	}
	if o.WallClockS == nil || *o.WallClockS != int64(4*time.Hour/time.Second) {
		t.Errorf("wall clock: opened T-1h, merged T+3h, want 14400 got %v", o.WallClockS)
	}
	if o.ReviewVerdict != "comment" || o.ReviewConfidence == nil || *o.ReviewConfidence != 0.7 {
		t.Errorf("review summary wrong: %+v", o)
	}
	if a.Quality == nil || a.Quality.ACMMLevel == nil || *a.Quality.ACMMLevel != 3 || a.Quality.RevertedWithin14d == nil || *a.Quality.RevertedWithin14d {
		t.Errorf("quality wrong: %+v", a.Quality)
	}

	r := byRole(recs, "hivecommons/hive", RoleReviewer)
	if r == nil || r.Task.Kind != "pr-review" || r.Agent.Name != reviewSwarmAgent || r.Model.Name != "claude-fable-5.1" {
		t.Errorf("reviewer record wrong: %+v", r)
	}

	p := byRole(recs, "", RoleAuthor)
	if p == nil {
		t.Fatal("missing anonymized record for private repo")
	}
	if p.Task.Repo != "" || p.Source.Org != "" || p.Task.RepoConsent || p.Task.RepoVisibility != "" {
		t.Errorf("private repo leaked: %+v %+v", p.Task, p.Source)
	}
	if p.SandboxRuntime != "podman" || p.Task.Kind != "issue-fix" {
		t.Errorf("stamped runtime/kind not honoured: %s %s", p.SandboxRuntime, p.Task.Kind)
	}
	if !p.Outcome.PRClosedUnmerged || p.Outcome.PRMerged {
		t.Errorf("ledger-closed PR should be closed unmerged: %+v", p.Outcome)
	}
	for _, rec := range recs {
		if strings.Contains(rec.Agent.Name, "andy") || strings.Contains(rec.Agent.Name, "[bot]") {
			t.Errorf("login leaked into %+v", rec.Agent)
		}
	}
}

func TestConvertRepoConsentOffAnonymizesPublic(t *testing.T) {
	opts := testOptions()
	opts.RepoConsent = false
	for _, r := range Convert(fixtureBundles(), opts) {
		if r.Task.Repo != "" || r.Source.Org != "" || r.Task.RepoConsent {
			t.Fatalf("repo_consent=false must drop names: %+v", r.Task)
		}
	}
}

func TestConvertSinceAndStableIDs(t *testing.T) {
	opts := testOptions()
	all := Convert(fixtureBundles(), opts)
	opts.Since = t0(4 * time.Hour)
	recent := Convert(fixtureBundles(), opts)
	if len(recent) != 1 || recent[0].Task.Repo != "" {
		t.Fatalf("since=T+4h should keep only the private PR updated at T+5h, got %d", len(recent))
	}
	again := Convert(fixtureBundles(), testOptions())
	for i := range all {
		if all[i].ID != again[i].ID {
			t.Fatalf("ids must be stable across runs")
		}
	}
	if HiveHash("hosted-test") == HiveHash("hosted-test2") || len(HiveHash("x")) != hiveHashLen {
		t.Fatal("hive hash shape")
	}
}

// TestConvertOutputMatchesSchema checks every emitted record against the
// vendored hivecommons/benchmarks schema: required keys, enum values and
// no unknown keys at each object the schema closes.
func TestConvertOutputMatchesSchema(t *testing.T) {
	raw, err := os.ReadFile("testdata/result.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for _, rec := range Convert(fixtureBundles(), testOptions()) {
		data, _ := json.Marshal(rec)
		var obj map[string]any
		_ = json.Unmarshal(data, &obj)
		checkSchema(t, "", schema, obj)
	}
}

func checkSchema(t *testing.T, path string, schema map[string]any, v any) {
	t.Helper()
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if e == v {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %v not in enum %v", path, v, enum)
		}
	}
	props, _ := schema["properties"].(map[string]any)
	obj, isObj := v.(map[string]any)
	if !isObj {
		return
	}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if _, present := obj[r.(string)]; !present {
				t.Errorf("%s: missing required %s", path, r)
			}
		}
	}
	for k, val := range obj {
		sub, ok := props[k].(map[string]any)
		if !ok {
			if ap, closed := schema["additionalProperties"].(bool); closed && !ap {
				t.Errorf("%s: unexpected key %s", path, k)
			}
			continue
		}
		if val == nil {
			continue
		}
		checkSchema(t, path+"."+k, sub, val)
	}
}

func TestConvertEdgeCases(t *testing.T) {
	// Backfilled merge (no Merge event), first bundle green, sentinel
	// findings, nameless agent on an unknown backend/model, and a bundle
	// that cannot be keyed — all with zero-value Options.
	ledgerMerged := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: "e", Repo: "o/r", Number: 9, HeadSHA: "s1",
		Author:    evidence.Author{Login: "bot", Kind: evidence.AuthorAgent, Backend: "mystery", Model: "zeta-9000"},
		CI:        evidence.CI{Status: "success"},
		Sentinel:  []evidence.SentinelFinding{{}},
		Outcome:   &evidence.Outcome{Source: "ledger", State: evidence.BackfillStateMerged, At: t0(4 * time.Hour)},
		Verdicts:  []evidence.Verdict{{Perspective: "security", Verdict: "approve"}}, // no model → no reviewer record
		CreatedAt: t0(2 * time.Hour), UpdatedAt: t0(4 * time.Hour),
	}
	earlier := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: "f", Repo: "o/r", Number: 9, HeadSHA: "s0",
		Author: ledgerMerged.Author, CI: evidence.CI{Status: "success"},
		CreatedAt: t0(0), UpdatedAt: t0(time.Hour),
	}
	unkeyed := &evidence.Bundle{SchemaVersion: evidence.SchemaVersion, ID: "g"}
	recs := Convert([]*evidence.Bundle{ledgerMerged, nil, unkeyed, earlier}, Options{HiveID: "x"})
	if len(recs) != 1 {
		t.Fatalf("want 1 author record, got %d: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.Agent.Name != "agent" || r.Agent.Harness != "other" || r.Model.Provider != "unknown" || r.Task.Kind != "unknown" {
		t.Errorf("defaults: %+v %+v %+v", r.Agent, r.Model, r.Task)
	}
	if r.SandboxRuntime != SandboxRuntimeNone || r.Source.HiveVersion == "" || r.PublishedAt == "" {
		t.Errorf("fallbacks: runtime=%q version=%q published=%q", r.SandboxRuntime, r.Source.HiveVersion, r.PublishedAt)
	}
	if !r.Outcome.PRMerged || r.Outcome.MergeMethod != evidence.MergeUnrecorded || r.Outcome.MergedAt == "" || r.Outcome.Retries != 1 {
		t.Errorf("ledger-merged outcome: %+v", r.Outcome)
	}
	if r.Outcome.CIGreenFirstTry == nil || !*r.Outcome.CIGreenFirstTry {
		t.Errorf("first bundle was green: %+v", r.Outcome.CIGreenFirstTry)
	}
	if r.Quality == nil || r.Quality.SentinelFindings == nil || *r.Quality.SentinelFindings != 1 {
		t.Errorf("quality: %+v", r.Quality)
	}

	// A merge event with an unknown method is recorded as unrecorded.
	odd := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: "h", Repo: "o/r", Number: 10, HeadSHA: "t1",
		Author:    evidence.Author{Kind: evidence.AuthorAgent, Model: "openai/gpt-5", Agent: "ci-fixer"},
		Policy:    evidence.Policy{ACMMLevel: 99},
		Merge:     &evidence.MergeEvent{Actor: "x", Method: "teleport", At: t0(time.Hour)},
		CreatedAt: t0(0), UpdatedAt: t0(time.Hour),
	}
	recs = Convert([]*evidence.Bundle{odd}, Options{HiveID: "x", Now: t0(2 * time.Hour)})
	if len(recs) != 1 || recs[0].Outcome.MergeMethod != evidence.MergeUnrecorded || recs[0].Model.Provider != "openai" || recs[0].Task.Kind != "ci-repair" {
		t.Errorf("odd merge: %+v", recs)
	}
	if recs[0].Quality != nil && recs[0].Quality.ACMMLevel != nil {
		t.Errorf("out-of-range ACMM level must be dropped: %+v", recs[0].Quality)
	}
	if got := qualityOf(&evidence.Bundle{Policy: evidence.Policy{ACMMLevel: 99}}, &evidence.PR{}); got != nil {
		t.Errorf("qualityOf with nothing to report = %+v, want nil", got)
	}
}
