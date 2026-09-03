package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDispatchStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "dispatch-state.json")
	state := DispatchState{
		GeneratedAt: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC),
		Pending: []PendingReview{
			{Repo: "hivecommons/hive", Number: 42, HeadSHA: testSHA, Perspective: PerspectiveSecurity, Agent: "sec-check"},
		},
		Fixes: []PendingFix{
			{Repo: "hivecommons/hive", Number: 42, HeadSHA: testSHA, Agent: "scanner", Attempts: 2},
		},
		Human: []HumanReviewHold{
			{Repo: "hivecommons/hive", Number: 43, HeadSHA: testSHA, Reason: "high severity finding"},
		},
	}
	if err := WriteDispatchState(path, state); err != nil {
		t.Fatalf("WriteDispatchState: %v", err)
	}
	got, err := LoadDispatchState(path)
	if err != nil {
		t.Fatalf("LoadDispatchState: %v", err)
	}
	if len(got.Pending) != 1 || got.Pending[0].Number != 42 || got.Pending[0].Perspective != PerspectiveSecurity {
		t.Errorf("pending round trip = %+v", got.Pending)
	}
	if len(got.Fixes) != 1 || got.Fixes[0].Attempts != 2 {
		t.Errorf("fixes round trip = %+v", got.Fixes)
	}
	if len(got.Human) != 1 || got.Human[0].Reason != "high severity finding" {
		t.Errorf("human round trip = %+v", got.Human)
	}
}

func TestLoadDispatchStateErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadDispatchState(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("missing file should error")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDispatchState(bad); err == nil {
		t.Error("malformed JSON should error")
	}
}

func TestWriteDispatchStateMkdirError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "occupied")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Parent path component is a regular file, so MkdirAll must fail.
	if err := WriteDispatchState(filepath.Join(file, "state.json"), DispatchState{}); err == nil {
		t.Error("write under a file path should error")
	}
}

func TestUpsertHuman(t *testing.T) {
	first := HumanReviewHold{Repo: "hivecommons/hive", Number: 7, HeadSHA: testSHA, Reason: "initial"}
	other := HumanReviewHold{Repo: "hivecommons/hive", Number: 8, HeadSHA: testSHA, Reason: "different PR"}

	items := upsertHuman(nil, first)
	if len(items) != 1 || items[0].Reason != "initial" {
		t.Fatalf("append to empty = %+v", items)
	}
	updated := first
	updated.Reason = "revised"
	items = upsertHuman(items, updated)
	if len(items) != 1 || items[0].Reason != "revised" {
		t.Errorf("in-place replace = %+v", items)
	}
	items = upsertHuman(items, other)
	if len(items) != 2 {
		t.Errorf("distinct head appends, got %+v", items)
	}
}

func TestRemovePendingForHead(t *testing.T) {
	pending := []PendingReview{
		{Repo: "hivecommons/hive", Number: 5, HeadSHA: testSHA, Perspective: PerspectiveSecurity},
		{Repo: "hivecommons/hive", Number: 5, HeadSHA: testSHA, Perspective: PerspectiveCorrectness},
		{Repo: "hivecommons/hive", Number: 6, HeadSHA: testSHA, Perspective: PerspectiveSecurity},
	}
	out := removePendingForHead(pending, PullRequest{Repo: "hivecommons/hive", Number: 5, HeadSHA: testSHA})
	if len(out) != 1 || out[0].Number != 6 {
		t.Errorf("removePendingForHead = %+v, want only PR 6", out)
	}
	out = removePendingForHead(out, PullRequest{Repo: "hivecommons/hive", Number: 99, HeadSHA: testSHA})
	if len(out) != 1 {
		t.Errorf("non-matching PR must remove nothing, got %+v", out)
	}
}

func TestReviewCapableAgents(t *testing.T) {
	agents := []AgentCapability{
		{Name: "zeta-review", Enabled: true, UsesKick: true, Role: "review"},
		{Name: "alpha", Enabled: true, UsesKick: true, LaneKeywords: []string{"review-swarm"}},
		{Name: "disabled", Enabled: false, UsesKick: true, Role: "review"},
		{Name: "paused", Enabled: true, Paused: true, UsesKick: true, Role: "review"},
		{Name: "on-demand", Enabled: true, OnDemand: true, UsesKick: true, Role: "review"},
		{Name: "no-kick", Enabled: true, Role: "review"},
		{Name: "scanner", Enabled: true, UsesKick: true, Role: "triage"},
		{Name: "", Enabled: true, UsesKick: true, Role: "review"},
	}

	got := reviewCapableAgents(DispatchOptions{Agents: agents})
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta-review" {
		t.Errorf("keyword capability filter = %+v, want sorted [alpha zeta-review]", got)
	}

	got = reviewCapableAgents(DispatchOptions{Agents: agents, ReviewerAgents: []string{" scanner ", ""}})
	if len(got) != 1 || got[0].Name != "scanner" {
		t.Errorf("explicit allow-list = %+v, want [scanner]", got)
	}
}

func TestValidateReportRejections(t *testing.T) {
	valid := baseReport(PerspectiveSecurity, VerdictApprove)
	mutate := func(fn func(r *PerspectiveReport)) []byte {
		r := valid
		fn(&r)
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"empty input", []byte("   \n"), "non-empty"},
		{"unknown field", []byte(`{"kind":"review","surprise":true}`), "decode"},
		{"trailing object", append(mustJSON(t, valid), mustJSON(t, valid)...), "exactly one"},
		{"wrong kind", mutate(func(r *PerspectiveReport) { r.Kind = "advisory" }), "kind"},
		{"bad perspective", mutate(func(r *PerspectiveReport) { r.Perspective = "vibes" }), "perspective"},
		{"bad verdict", mutate(func(r *PerspectiveReport) { r.Verdict = "maybe" }), "verdict"},
		{"missing repo", mutate(func(r *PerspectiveReport) { r.Repo = "  " }), "repo"},
		{"unqualified repo", mutate(func(r *PerspectiveReport) { r.Repo = "hive" }), "owner/name"},
		{"non-positive number", mutate(func(r *PerspectiveReport) { r.Number = 0 }), "number"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateReport(tt.raw)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ValidateReport error = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCollectErrors(t *testing.T) {
	if _, err := Collect(filepath.Join(t.TempDir(), "absent"), AggregateOptions{}); err == nil {
		t.Error("missing report dir should error")
	}

	dir := t.TempDir()
	name := ReviewReportFilePrefix + "bad" + ReviewReportFileSuffix
	if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"kind":"nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(dir, AggregateOptions{}); err == nil || !strings.Contains(err.Error(), name) {
		t.Errorf("invalid report must fail naming the file, got %v", err)
	}
}

func TestCollectSkipsNonReportEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ReviewReportFilePrefix+"sub"+ReviewReportFileSuffix), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := baseReport(PerspectiveSecurity, VerdictApprove)
	if err := os.WriteFile(filepath.Join(dir, ReviewReportFilePrefix+"ok"+ReviewReportFileSuffix), mustJSON(t, report), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, err := Collect(dir, AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 || artifact.Items[0].Number != report.Number {
		t.Errorf("Collect items = %+v, want the single valid report", artifact.Items)
	}
}
