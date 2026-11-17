package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/modelfamily"
)

// writeCanonicalRunLog writes run records under several spellings of the
// same models: claude-opus-5-5 with and without a provider prefix, and the
// Ornith model in two cases (#18718).
func writeCanonicalRunLog(t *testing.T, dir string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	recs := []TaskRunRecord{
		{TS: now, Model: "claude-opus-5-5", Outcome: outcomeCompleted, PRVerified: true},
		{TS: now, Model: "claude-opus-5-5", Outcome: outcomeFailed},
		{TS: now, Model: "claude-opus-5-5", Outcome: outcomeCompleted},
		{TS: now, Model: "anthropic/claude-opus-5-5", Outcome: outcomeCompleted, PRVerified: true},
		{TS: now, Model: "lemonade/Ornith-1.5-35B-A3B-GGUF-Q6_K", Outcome: outcomeFailed},
		{TS: now, Model: "lemonade/ornith-1.5-35b-a3b-gguf-q6_k", Outcome: outcomeCompleted},
		{TS: now, Model: "m2", Outcome: outcomeCompleted},
	}
	var buf bytes.Buffer
	for _, r := range recs {
		b, _ := json.Marshal(r)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, taskRunLogFileName), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runStatsModelRows(t *testing.T, s *Server, url string) []map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handleContributeRunStatsByModel(rr, httptest.NewRequest("GET", url, nil))
	body := decodeWallResp(t, rr)
	raw := body["models"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

func TestContributeRunStatsByModelCanonicalField(t *testing.T) {
	s, dir := newWallTestServer(t, true)
	writeCanonicalRunLog(t, dir)
	rows := runStatsModelRows(t, s, "/api/contribute/run-stats/models?days=1")
	if len(rows) != 4 {
		t.Fatalf("ungrouped rows = %d, want 4 (raw spellings stay separate): %#v", len(rows), rows)
	}
	byModel := map[string]map[string]any{}
	for _, row := range rows {
		model := row["model"].(string)
		byModel[model] = row
		if got, want := row["canonical_model"], modelfamily.Canonical(model); got != want {
			t.Errorf("%s canonical_model = %v, want %q", model, got, want)
		}
		if _, ok := row["raw_models"]; ok {
			t.Errorf("%s: raw_models must be absent without group=canonical", model)
		}
		for _, k := range []string{"runs", "verified_pr_share", "failure_rate"} {
			if _, ok := row[k].(float64); !ok {
				t.Errorf("%s: existing field %s missing or retyped: %#v", model, k, row)
			}
		}
	}
	opus := byModel["claude-opus-5-5"]
	if opus == nil || opus["runs"].(float64) != 3 || opus["verified_pr_share"].(float64) != 1.0/3 || opus["failure_rate"].(float64) != 1.0/3 {
		t.Fatalf("claude-opus-5-5 row changed: %#v", opus)
	}
	if opus["canonical_model"] != "claude-opus-5.5" {
		t.Fatalf("claude-opus-5-5 canonical = %v", opus["canonical_model"])
	}
	if pfx := byModel["anthropic/claude-opus-5-5"]; pfx == nil || pfx["canonical_model"] != "claude-opus-5.5" || pfx["runs"].(float64) != 1 {
		t.Fatalf("anthropic/claude-opus-5-5 row: %#v", pfx)
	}
	// The two Ornith cases were already one row (case-insensitive key); the
	// first spelling seen is displayed, as before.
	orn := byModel["lemonade/Ornith-1.5-35B-A3B-GGUF-Q6_K"]
	if orn == nil || orn["runs"].(float64) != 2 || orn["canonical_model"] != "ornith-1.5-35b-a3b-gguf-q6-k" {
		t.Fatalf("ornith row: %#v", orn)
	}
}

func TestContributeRunStatsByModelGroupCanonical(t *testing.T) {
	s, dir := newWallTestServer(t, true)
	writeCanonicalRunLog(t, dir)
	plain := runStatsModelRows(t, s, "/api/contribute/run-stats/models?days=1")
	grouped := runStatsModelRows(t, s, "/api/contribute/run-stats/models?days=1&group=canonical")
	if len(grouped) != 3 {
		t.Fatalf("grouped rows = %d, want 3: %#v", len(grouped), grouped)
	}
	sum := func(rows []map[string]any) float64 {
		n := 0.0
		for _, r := range rows {
			n += r["runs"].(float64)
		}
		return n
	}
	if sum(plain) != sum(grouped) || sum(grouped) != 7 {
		t.Fatalf("runs: plain=%v grouped=%v, want both 7", sum(plain), sum(grouped))
	}
	byModel := map[string]map[string]any{}
	for _, row := range grouped {
		if row["model"] != row["canonical_model"] {
			t.Errorf("grouped row model %v != canonical_model %v", row["model"], row["canonical_model"])
		}
		byModel[row["model"].(string)] = row
	}
	opus := byModel["claude-opus-5.5"]
	if opus == nil {
		t.Fatalf("no claude-opus-5.5 row: %#v", grouped)
	}
	// 4 runs: 2 verified (one per spelling), 1 failed. Rates are the
	// summed-count ratios, not an average of the per-spelling rates.
	if opus["runs"].(float64) != 4 || opus["verified_pr_share"].(float64) != 0.5 || opus["failure_rate"].(float64) != 0.25 {
		t.Fatalf("claude-opus-5.5 grouped stats: %#v", opus)
	}
	raws := opus["raw_models"].([]any)
	if len(raws) != 2 {
		t.Fatalf("raw_models = %#v", raws)
	}
	first, second := raws[0].(map[string]any), raws[1].(map[string]any)
	if first["model"] != "claude-opus-5-5" || first["runs"].(float64) != 3 || second["model"] != "anthropic/claude-opus-5-5" || second["runs"].(float64) != 1 {
		t.Fatalf("raw_models order/counts = %#v", raws)
	}
	orn := byModel["ornith-1.5-35b-a3b-gguf-q6-k"]
	if orn == nil || orn["runs"].(float64) != 2 || orn["failure_rate"].(float64) != 0.5 {
		t.Fatalf("ornith grouped: %#v", orn)
	}
	if m2 := byModel["m2"]; m2 == nil || m2["runs"].(float64) != 1 || len(m2["raw_models"].([]any)) != 1 {
		t.Fatalf("m2 grouped: %#v", m2)
	}

	// Any other group value keeps the ungrouped rows.
	other := runStatsModelRows(t, s, "/api/contribute/run-stats/models?days=1&group=raw")
	if len(other) != len(plain) {
		t.Fatalf("group=raw rows = %d, want %d", len(other), len(plain))
	}
}

func TestContributeRunStatsModelsGroupCanonical(t *testing.T) {
	s, dir := newWallTestServer(t, true)
	writeCanonicalRunLog(t, dir)
	get := func(url string) []any {
		rr := httptest.NewRecorder()
		s.handleContributeRunStats(rr, httptest.NewRequest("GET", url, nil))
		return decodeWallResp(t, rr)["models"].([]any)
	}
	if n := len(get("/api/contribute/run-stats?days=1")); n != 4 {
		t.Fatalf("run-stats models ungrouped = %d, want 4", n)
	}
	grouped := get("/api/contribute/run-stats?days=1&group=canonical")
	if len(grouped) != 3 {
		t.Fatalf("run-stats models grouped = %d, want 3", len(grouped))
	}
	for _, m := range grouped {
		row := m.(map[string]any)
		if _, ok := row["raw_models"]; !ok {
			t.Errorf("grouped run-stats row lacks raw_models: %#v", row)
		}
	}
}

func TestGroupWallModelStatsByCanonicalUnit(t *testing.T) {
	rows := []WallModelStats{
		{Model: "openai-codex/gpt-6.1-sol", Runs: 2, CanonicalModel: "gpt-6.1-sol", verifiedRuns: 2},
		{Model: "gpt-6.1-sol", Runs: 2, CanonicalModel: "gpt-6.1-sol", failedRuns: 1},
		{Model: "openai/gpt-6.1-sol", Runs: 0},
	}
	out := groupWallModelStatsByCanonical(rows)
	if len(out) != 1 {
		t.Fatalf("out = %#v", out)
	}
	g := out[0]
	if g.Model != "gpt-6.1-sol" || g.Runs != 4 || g.VerifiedPRShare != 0.5 || g.FailureRate != 0.25 || len(g.RawModels) != 3 {
		t.Fatalf("grouped = %#v", g)
	}
	if g.RawModels[0].Model != "gpt-6.1-sol" || g.RawModels[1].Model != "openai-codex/gpt-6.1-sol" || g.RawModels[2].Runs != 0 {
		t.Fatalf("raw order = %#v", g.RawModels)
	}
	if got := groupWallModelStatsByCanonical(nil); len(got) != 0 {
		t.Fatalf("nil input = %#v", got)
	}
}

// group=canonical sums the #18721 run-cause split along with the counts, so a
// merged row's scenarios still sum to runs and failure_kinds to the failures.
func TestGroupWallModelStatsByCanonicalSumsRunCauses(t *testing.T) {
	rows := []WallModelStats{
		{Model: "claude-opus-5-5", Runs: 3, CanonicalModel: "claude-opus-5-5", failedRuns: 1, Abandoned: 1,
			Scenarios:    map[string]int{scenarioEnvFailure: 1, "abandoned_handback": 1, "clean_complete": 1},
			FailureKinds: WallModelFailureKinds{Environment: 1}},
		{Model: "anthropic/claude-opus-5-5", Runs: 2, CanonicalModel: "claude-opus-5-5", failedRuns: 1,
			Scenarios:    map[string]int{scenarioTaskFailure: 1, "clean_complete": 1},
			FailureKinds: WallModelFailureKinds{Task: 1}},
	}
	out := groupWallModelStatsByCanonical(rows)
	if len(out) != 1 {
		t.Fatalf("out = %#v", out)
	}
	g := out[0]
	if g.Abandoned != 1 || g.FailureKinds != (WallModelFailureKinds{Environment: 1, Task: 1}) {
		t.Fatalf("grouped causes = %#v", g)
	}
	sum := 0
	for _, n := range g.Scenarios {
		sum += n
	}
	if sum != g.Runs || g.Scenarios["clean_complete"] != 2 {
		t.Fatalf("scenarios = %#v, runs %d", g.Scenarios, g.Runs)
	}
}
