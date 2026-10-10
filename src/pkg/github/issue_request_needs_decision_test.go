package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// needsDecisionFilter is an IssueAdmitter that names a configured
// needs-decision label, standing in for config.IssueFilterConfig.
type needsDecisionFilter struct{ label string }

func (needsDecisionFilter) Admits([]string) bool { return true }
func (f needsDecisionFilter) NeedsDecisionLabel() string { return f.label }

// newIssueCreateLabelServer mocks the create path and records the labels sent
// with each POST /issues.
func newIssueCreateLabelServer(t *testing.T, mu *sync.Mutex, got *[][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues"):
			var body struct {
				Labels []string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*got = append(*got, body.Labels)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":990099,"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func runNeedsDecisionCreate(t *testing.T, filter IssueAdmitter, req IssueRequest) []string {
	t.Helper()
	var mu sync.Mutex
	var got [][]string
	srv := newIssueCreateLabelServer(t, &mu, &got)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	if filter != nil {
		c.SetIssueFilter(filter)
	}
	dir := withIssueDir(t)
	if _, err := WriteIssueRequest(dir, req); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 issue create, got %d", len(got))
	}
	return got[0]
}

func hasLabelFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

// needs_decision=true makes the relay apply the configured needs-decision
// label itself, even though the agent did not name it (#11215).
func TestIssueRequestWatcher_NeedsDecisionAppliesConfiguredLabel(t *testing.T) {
	labels := runNeedsDecisionCreate(t, needsDecisionFilter{label: "owner-call"}, IssueRequest{
		Repo: "o/r", Title: "[architect] Pick A or B", Body: "the maintainer picks A or B",
		Labels: []string{"agent/architect"}, Agent: "architect", NeedsDecision: true,
	})
	if !hasLabelFold(labels, "owner-call") {
		t.Errorf("configured needs-decision label not applied; create labels = %v", labels)
	}
	if !hasLabelFold(labels, "agent/architect") {
		t.Errorf("agent's own labels dropped; create labels = %v", labels)
	}
	if hasLabelFold(labels, issueNeedsDecisionLabel) {
		t.Errorf("default label applied although a different one is configured; create labels = %v", labels)
	}
}

// With no configured filter the built-in needs-decision label is used.
func TestIssueRequestWatcher_NeedsDecisionDefaultsLabel(t *testing.T) {
	labels := runNeedsDecisionCreate(t, nil, IssueRequest{
		Repo: "o/r", Title: "[architect] Pick A or B", Body: "the maintainer picks A or B",
		Agent: "architect", NeedsDecision: true,
	})
	if !hasLabelFold(labels, issueNeedsDecisionLabel) {
		t.Errorf("default needs-decision label not applied; create labels = %v", labels)
	}
}

// Without needs_decision the relay passes the agent's labels through as given.
func TestIssueRequestWatcher_NoNeedsDecisionLeavesLabels(t *testing.T) {
	labels := runNeedsDecisionCreate(t, needsDecisionFilter{label: "owner-call"}, IssueRequest{
		Repo: "o/r", Title: "[architect] Refactor X", Body: "plain finding",
		Labels: []string{"agent/architect"}, Agent: "architect",
	})
	if hasLabelFold(labels, "owner-call") || hasLabelFold(labels, issueNeedsDecisionLabel) {
		t.Errorf("needs-decision label applied without the flag; create labels = %v", labels)
	}
}

// An agent that both sets the flag and names the label gets it once.
func TestIssueRequestWatcher_NeedsDecisionDoesNotDuplicateLabel(t *testing.T) {
	labels := runNeedsDecisionCreate(t, nil, IssueRequest{
		Repo: "o/r", Title: "[quality] Pick A or B", Body: "the maintainer picks A or B",
		Labels: []string{"Needs-Decision"}, Agent: "quality", NeedsDecision: true,
	})
	n := 0
	for _, l := range labels {
		if strings.EqualFold(l, issueNeedsDecisionLabel) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("needs-decision label applied %d times; create labels = %v", n, labels)
	}
}
