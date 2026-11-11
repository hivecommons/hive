package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
)

func simulateCall(t *testing.T, s *Server, path string, body any) (int, SimulateResponse, string) {
	t.Helper()
	rec := wfReq(s, http.MethodPost, path, body, false)
	var out SimulateResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v\n%s", err, rec.Body)
		}
	}
	return rec.Code, out, rec.Body.String()
}

// hive-default (derivation builtin): a green, approved PR by a trusted
// author derives the built-in 080 and the trace is the single builtin
// entry naming the state and column the profile puts it in.
func TestWorkflowSimulateHiveDefaultPR(t *testing.T) {
	s := workflowTestServer(t, true)
	code, out, raw := simulateCall(t, s, "/api/workflows/hive-default/1/simulate", map[string]any{
		"kind": "pr", "repo": "myorg/repo1", "number": 7,
		"facts": map[string]any{
			"labels": []string{"role/trusted"}, "open": true,
			"author": map[string]any{"login": "andy", "kind": "human", "role": "trusted"},
			"pr":     map[string]any{"checks": "green", "review": "approved", "dco": "yes"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	d := out.Decision
	if d.Abstain || !d.Covered || d.Stage != 80 || d.State != "awaiting-merge-admission" || d.Column == "" || !strings.HasPrefix(d.Label, "stage/080") {
		t.Fatalf("decision %+v", d)
	}
	if len(out.Trace) != 1 || !out.Trace[0].Builtin || !out.Trace[0].Matched || out.Trace[0].State != d.State {
		t.Fatalf("trace %+v", out.Trace)
	}
	if out.Facts.PR == nil || out.Facts.PR.Lane == nil || out.Facts.PR.Lane.Lane != "trusted-author" || len(out.Notes) != 1 {
		t.Fatalf("simulated lane not reported: %+v notes=%v", out.Facts.PR, out.Notes)
	}
	if out.Facts.Issue != nil || out.Facts.PR.Review != "approved" || out.Facts.Author.Kind != "human" {
		t.Fatalf("facts not normalized: %+v", out.Facts)
	}

	// Red CI on the hive's own PR is the fixing stage.
	code, out, raw = simulateCall(t, s, "/api/workflows/hive-default/1/simulate", map[string]any{
		"kind":  "pr",
		"facts": map[string]any{"open": true, "author": map[string]any{"kind": "hive", "selfAuthored": true, "role": "trusted"}, "pr": map[string]any{"checks": "red"}},
	})
	if code != http.StatusOK || out.Decision.Stage != 70 {
		t.Fatalf("%d %+v %s", code, out.Decision, raw)
	}

	// An issue needs no PR evidence.
	code, out, raw = simulateCall(t, s, "/api/workflows/hive-default/1/simulate", map[string]any{
		"kind": "issue", "facts": map[string]any{"open": true, "author": map[string]any{"kind": "human"}, "issue": map[string]any{"bucket": "actionable"}},
	})
	if code != http.StatusOK || out.Decision.Stage != 10 || out.Decision.State != "actionable" {
		t.Fatalf("%d %+v %s", code, out.Decision, raw)
	}
}

// A rules model: the trace lists every rule in order, marks the first
// match, shows each predicate's verdict and actual value, and leaves the
// rules after the match unevaluated.
func TestWorkflowSimulateRulesTrace(t *testing.T) {
	s := workflowTestServer(t, true)
	if rec := wfReq(s, http.MethodPost, "/api/workflows", techdocsModel(t), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	code, out, raw := simulateCall(t, s, "/api/workflows/cncf-techdocs/1/simulate", map[string]any{
		"kind": "issue",
		"facts": map[string]any{
			"labels": []string{"techdocs/request", "techdocs/accepted"}, "open": true,
			"author": map[string]any{"login": "nate", "kind": "human"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	if out.Decision.Abstain || out.Decision.Stage != 20 || out.Decision.Column == "" {
		t.Fatalf("decision %+v", out.Decision)
	}
	var matched, afterMatch int
	seenMatch := false
	for _, tr := range out.Trace {
		if tr.Builtin {
			t.Fatalf("rules trace has a builtin entry: %+v", tr)
		}
		if tr.Matched {
			matched++
			seenMatch = true
			if tr.State != out.Decision.State {
				t.Fatalf("matched rule %s names %s, decision %s", tr.ID, tr.State, out.Decision.State)
			}
			for _, p := range tr.Predicates {
				if !p.Holds || !p.Known || p.Fact == "" || p.Op == "" {
					t.Fatalf("matched rule has a failing/unknown predicate: %+v", p)
				}
			}
			continue
		}
		if seenMatch {
			afterMatch++
			if tr.Evaluated || len(tr.Predicates) != 0 {
				t.Fatalf("rule after the match was evaluated: %+v", tr)
			}
		} else if tr.Evaluated {
			failing := 0
			for _, p := range tr.Predicates {
				if !p.Holds {
					failing++
				}
			}
			if failing == 0 {
				t.Fatalf("evaluated, unmatched rule has no failing predicate: %+v", tr)
			}
		}
	}
	if matched != 1 || afterMatch == 0 {
		t.Fatalf("matched=%d afterMatch=%d trace=%+v", matched, afterMatch, out.Trace)
	}

	// Uncovered facts abstain with the not-covered reason.
	code, out, raw = simulateCall(t, s, "/api/workflows/cncf-techdocs/1/simulate", map[string]any{
		"kind": "issue", "facts": map[string]any{"labels": []string{"bug"}, "open": true},
	})
	if code != http.StatusOK || !out.Decision.Abstain || out.Decision.Covered || !strings.HasPrefix(out.Decision.Reason, stageengine.ReasonNotCovered) {
		t.Fatalf("%d %+v %s", code, out.Decision, raw)
	}
	for _, tr := range out.Trace {
		if tr.Matched {
			t.Fatalf("uncovered run has a matched rule: %+v", tr)
		}
	}
}

func TestWorkflowSimulateErrors(t *testing.T) {
	s := workflowTestServer(t, true)
	for _, c := range []struct {
		name string
		path string
		body any
		code int
	}{
		{"bad kind", "/api/workflows/hive-default/1/simulate", map[string]any{"kind": "epic", "facts": map[string]any{}}, http.StatusBadRequest},
		{"no kind", "/api/workflows/hive-default/1/simulate", map[string]any{"facts": map[string]any{}}, http.StatusBadRequest},
		{"bad json", "/api/workflows/hive-default/1/simulate", "{", http.StatusBadRequest},
		{"unknown model", "/api/workflows/nope/1/simulate", map[string]any{"kind": "pr", "facts": map[string]any{}}, http.StatusNotFound},
		{"neither facts nor item", "/api/workflows/hive-default/1/simulate", map[string]any{"kind": "pr"}, http.StatusBadRequest},
		{"item without forge client", "/api/workflows/hive-default/1/simulate", map[string]any{"kind": "pr", "repo": "myorg/repo1", "number": 3}, http.StatusUnprocessableEntity},
	} {
		if code, _, raw := simulateCall(t, s, c.path, c.body); code != c.code {
			t.Errorf("%s: %d want %d: %s", c.name, code, c.code, raw)
		}
	}
	if rec := wfReq(workflowTestServer(t, false), http.MethodPost, "/api/workflows/hive-default/1/simulate", map[string]any{"kind": "pr", "facts": map[string]any{}}, false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store: %d", rec.Code)
	}
}

// repo+number without facts fills from the forge snapshot and reports
// what was filled and what stays unknown; the kind comes from the item.
func TestWorkflowSimulateFetchesItem(t *testing.T) {
	s := workflowTestServer(t, true)
	var asked string
	s.deps.WorkflowItemSnapshot = func(_ context.Context, repo string, number int) (ghpkg.WorkflowItemSnapshot, error) {
		asked = repo + "#" + string(rune('0'+number))
		if number == 9 {
			return ghpkg.WorkflowItemSnapshot{}, errors.New("boom")
		}
		return ghpkg.WorkflowItemSnapshot{
			Kind: "pr", Labels: []string{"role/untrusted", "hold", "origin/scanner"}, Open: true, Author: "nate",
			HeadSHA: "abc", BaseRef: "main", ConflictKnown: true,
			Checks: ghpkg.CommitChecksSummary{Total: 2, Green: 2, DCO: 1}, ChecksKnown: true,
		}, nil
	}
	code, out, raw := simulateCall(t, s, "/api/workflows/hive-default/1/simulate", map[string]any{"repo": "myorg/repo1", "number": 4})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	if asked != "myorg/repo1#4" {
		t.Fatalf("asked %q", asked)
	}
	f := out.Facts
	if f.PR == nil || f.Issue != nil || f.PR.Checks != "green" || f.PR.DCO != "yes" || f.PR.Review != "pending" || f.Author.Role != "untrusted" || f.Author.Login != "nate" || f.Origin != "origin/scanner" {
		t.Fatalf("facts %+v pr=%+v", f, f.PR)
	}
	if len(f.Park.Holds) != 1 || f.Park.Holds[0] != "hold" {
		t.Fatalf("holds %v", f.Park.Holds)
	}
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(out.Filled, "pr.checks") || !has(out.Filled, "labels") || !has(out.Unknown, "pr.review") || !has(out.Unknown, "author.kind") || has(out.Unknown, "pr.conflicting") {
		t.Fatalf("filled=%v unknown=%v", out.Filled, out.Unknown)
	}
	if out.Decision.Abstain {
		t.Fatalf("decision %+v", out.Decision)
	}

	if code, _, raw := simulateCall(t, s, "/api/workflows/hive-default/1/simulate", map[string]any{"kind": "issue", "repo": "myorg/repo1", "number": 4}); code != http.StatusBadRequest || !strings.Contains(raw, "is a pr") {
		t.Fatalf("kind mismatch: %d %s", code, raw)
	}
	if code, _, _ := simulateCall(t, s, "/api/workflows/hive-default/1/simulate", map[string]any{"repo": "myorg/repo1", "number": 9}); code != http.StatusBadGateway {
		t.Fatalf("forge error: %d", code)
	}
}
