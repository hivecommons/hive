package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestACMMDecodesFixture decodes testdata/acmm.json and asserts every field of
// both packs, including the whole 20-field agent roster on the entry that
// carries all of them.
//
// The Level assertion is the one that is not a plain decode: GET /api/packs
// returns a bare array with no level field, so the active level exists only as
// the `current` flag on one entry.
func TestACMMDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "acmm.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").ACMM(context.Background())
	if err != nil {
		t.Fatalf("ACMM() = %v, want nil", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/packs" {
		t.Errorf("path = %q, want /api/packs", gotPath)
	}

	if got.Level != 4 {
		t.Errorf("Level = %d, want 4 — derived from the pack flagged current", got.Level)
	}
	if len(got.Packs) != 2 {
		t.Fatalf("Packs = %d entries, want 2", len(got.Packs))
	}

	l1 := got.Packs[0]
	if l1.Level != 1 || l1.Name != "Observe" || l1.AgentCount != 1 || l1.Current {
		t.Errorf("pack[0] = %+v, want level 1 Observe agentCount 1 current false", l1)
	}
	if l1.Governor.Modes != "advisory" || l1.Governor.MergePolicy != "manual" {
		t.Errorf("pack[0].Governor = %+v, want advisory/manual", l1.Governor)
	}
	// Absent optional governor fields decode to their zero values rather than
	// failing: most packs carry no cadences or thresholds at all.
	if l1.Governor.EvalIntervalS != 0 || l1.Governor.Cadences != nil || l1.Governor.Thresholds != nil || l1.Governor.PlanAutoApprove {
		t.Errorf("pack[0].Governor = %+v, want zero values for the absent fields", l1.Governor)
	}

	l4 := got.Packs[1]
	if l4.Level != 4 || !l4.Current || l4.AgentCount != 2 {
		t.Errorf("pack[1] = level %d current %v agentCount %d, want 4/true/2", l4.Level, l4.Current, l4.AgentCount)
	}
	wantGov := PackGovernor{
		Modes:         "issues_and_prs",
		MergePolicy:   "owner",
		EvalIntervalS: 60,
		Cadences: map[string]map[string]string{
			"quiet": {"scanner": "30m"},
			"busy":  {"scanner": "5m"},
		},
		Thresholds:      map[string]int{"quiet": 0, "busy": 5, "surge": 20},
		PlanAutoApprove: false,
	}
	if !reflect.DeepEqual(l4.Governor, wantGov) {
		t.Errorf("pack[1].Governor = %+v, want %+v", l4.Governor, wantGov)
	}

	if len(l4.Agents) != 2 {
		t.Fatalf("pack[1].Agents = %d, want 2", len(l4.Agents))
	}
	// The second agent carries every field the type defines, including the
	// five that are omitempty on the wire — so this asserts the full contract.
	wantAgent := PackAgent{
		Name:         "telemetry",
		DisplayName:  "Telemetry",
		Role:         "telemetry",
		Description:  "Watches metrics and reports drift.",
		Emoji:        "📈",
		Color:        "#f0a30a",
		SortOrder:    9,
		Backend:      "copilot",
		Model:        "gpt-5",
		BeadRole:     "advisory",
		KickTemplate: "telemetry",
		IncludeRepos: false,
		LaneKeywords: []string{"metrics"},
		Interactions: "Reports to the operations agent.",
		KnowledgeUse: "Writes observations back.",
		Hidden:       true,
		StaleTimeout: 900,
		Mode:         "advisory",
		OnDemand:     true,
		CavemanMode:  "off",
	}
	if !reflect.DeepEqual(l4.Agents[1], wantAgent) {
		t.Errorf("pack[1].Agents[1] =\n %+v\nwant\n %+v", l4.Agents[1], wantAgent)
	}

	current, ok := got.Current()
	if !ok {
		t.Fatal("Current() reported no current pack, want the level-4 one")
	}
	if current.Level != 4 {
		t.Errorf("Current().Level = %d, want 4", current.Level)
	}
}

// TestACMMNoCurrentPack: a hive with no level configured flags no pack, which
// is an ordinary state and not an error. Level must read 0 rather than
// defaulting to whatever pack happened to come first.
func TestACMMNoCurrentPack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"level":1,"name":"Observe","agentCount":0,"current":false},{"level":2,"name":"Advise","agentCount":0,"current":false}]`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").ACMM(context.Background())
	if err != nil {
		t.Fatalf("ACMM() = %v, want nil", err)
	}
	if got.Level != 0 {
		t.Errorf("Level = %d, want 0 when no pack is current", got.Level)
	}
	if _, ok := got.Current(); ok {
		t.Error("Current() reported a pack, want none")
	}
	if len(got.Packs) != 2 {
		t.Errorf("Packs = %d, want the packs to survive regardless", len(got.Packs))
	}
}

func TestACMMNonOKReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"ok":false,"error":"boom"}`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").ACMM(context.Background())
	if err == nil {
		t.Fatal("ACMM() error = nil, want *APIError")
	}
	if got.Level != 0 || got.Packs != nil {
		t.Errorf("result = %+v, want the zero value on error", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
	if apiErr.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", apiErr.Method)
	}
}

// TestApplyACMMSendsExpectedRequest pins the whole write contract: the method
// (PUT, not POST — the mux matches on it, so a POST never reaches the handler),
// the path, the JSON body shape, and the Content-Type that must accompany it.
func TestApplyACMMSendsExpectedRequest(t *testing.T) {
	var gotPath, gotMethod, gotContentType string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"level":5,"packAgents":["scanner","strategist"],"packUpdated":["scanner"],"paused":["telemetry"],"resumed":["strategist"],"governor_changes":{"eval_interval_s":{"from":60,"to":30},"cadences":[{"mode":"busy","agent":"scanner","from":"5m","to":"2m"}]}}`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").ApplyACMM(context.Background(), 5)
	if err != nil {
		t.Fatalf("ApplyACMM() = %v, want nil", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/packs/level" {
		t.Errorf("path = %q, want /api/packs/level", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json for a request WITH a body", gotContentType)
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("request body %q is not JSON: %v", gotBody, err)
	}
	if !reflect.DeepEqual(sent, map[string]any{"level": float64(5)}) {
		t.Errorf("request body = %v, want exactly {\"level\":5}", sent)
	}

	want := ACMMLevelResult{
		OK:          true,
		Level:       5,
		PackAgents:  []string{"scanner", "strategist"},
		PackUpdated: []string{"scanner"},
		GovernorChanges: &GovernorChanges{
			EvalIntervalS: &GovernorIntervalChange{From: 60, To: 30},
			Cadences:      []GovernorCadenceChange{{Mode: "busy", Agent: "scanner", From: "5m", To: "2m"}},
		},
		Paused:  []string{"telemetry"},
		Resumed: []string{"strategist"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyACMM() =\n %+v\nwant\n %+v", got, want)
	}
}

// A level change that moved no governor setting omits governor_changes; the
// pointer must stay nil rather than decoding to a zero struct, so a caller can
// tell "nothing moved" from "moved to zero".
func TestApplyACMMAbsentGovernorChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"level":2,"packAgents":["scanner"],"packUpdated":null,"paused":null,"resumed":null}`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").ApplyACMM(context.Background(), 2)
	if err != nil {
		t.Fatalf("ApplyACMM() = %v, want nil", err)
	}
	if got.GovernorChanges != nil {
		t.Errorf("GovernorChanges = %+v, want nil when the level moved no setting", got.GovernorChanges)
	}
	if got.Level != 2 || !got.OK {
		t.Errorf("got %+v, want ok level 2", got)
	}
}

// TestApplyACMMNonOKReturnsAPIError covers every failure this operation
// documents. The 500 is the interesting one and is NOT a no-op: the level was
// already persisted and only the roster reconciliation failed, which the
// handler reports precisely so an operator sees the drift.
func TestApplyACMMNonOKReturnsAPIError(t *testing.T) {
	cases := []struct {
		name        string
		code        int
		body        string
		wantForbid  bool
		wantMessage string
	}{
		{"level out of range", http.StatusBadRequest, `{"ok":false,"error":"level must be 1-6"}`, false, "level must be 1-6"},
		{"owner gate", http.StatusForbidden, `{"ok":false,"error":"owner access required"}`, true, "owner access required"},
		{"reconciliation failed after persist", http.StatusInternalServerError, `{"ok":false,"error":"level set but roster reconciliation failed: boom"}`, false, "roster reconciliation failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			got, err := newTestClient(t, server, "tok").ApplyACMM(context.Background(), 9)
			if err == nil {
				t.Fatalf("ApplyACMM() error = nil, want *APIError for %d", tc.code)
			}
			if !reflect.DeepEqual(got, ACMMLevelResult{}) {
				t.Errorf("result = %+v, want the zero value on error", got)
			}

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.code {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.code)
			}
			if apiErr.Method != http.MethodPut {
				t.Errorf("Method = %q, want PUT", apiErr.Method)
			}
			// The server's own message is what tells an operator which of these
			// happened — especially the 500, whose text says the level WAS set.
			if !strings.Contains(apiErr.Error(), tc.wantMessage) {
				t.Errorf("Error() = %q, does not carry the server's message %q", apiErr.Error(), tc.wantMessage)
			}
			if IsForbidden(err) != tc.wantForbid {
				t.Errorf("IsForbidden = %v, want %v", IsForbidden(err), tc.wantForbid)
			}
		})
	}
}

// The client deliberately does not duplicate the server's 1-6 range check, so
// an out-of-range level must still reach the server and come back as its 400.
// This pins that choice: if a client-side guard is ever added, this fails.
func TestApplyACMMDoesNotValidateRangeLocally(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error":"level must be 1-6"}`)
	}))
	defer server.Close()

	if _, err := newTestClient(t, server, "tok").ApplyACMM(context.Background(), 99); err == nil {
		t.Fatal("ApplyACMM(99) = nil, want the server's 400")
	}
	if !strings.Contains(string(gotBody), "99") {
		t.Errorf("request body = %q; the level must reach the server, which owns the range", gotBody)
	}
}

func TestACMMMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"not":"an array"}`)
	}))
	defer server.Close()

	_, err := newTestClient(t, server, "tok").ACMM(context.Background())
	if err == nil {
		t.Fatal("ACMM() = nil, want a decode error")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("decode failure surfaced as *APIError (%v); the response WAS a 200", err)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, does not name the decode failure", err)
	}
}
