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
		HumanActions: []evidence.Action{{Actor: "andy", Kind: "label", At: t0(2 * time.Hour)}},
		Merge:        &evidence.MergeEvent{Actor: "hive", Method: "squash", At: t0(3 * time.Hour), OpenedAt: t0(-time.Hour)},
		CreatedAt:    t0(2 * time.Hour), UpdatedAt: t0(3 * time.Hour),
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
