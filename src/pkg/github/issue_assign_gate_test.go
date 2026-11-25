package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// assignGateServer serves one issue read for o/r#42.
func assignGateServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/o/r/issues/42" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The relay's live gate refuses a held issue — the hold family by substring
// (do-not-merge/hold) and an operator's configured hold label alike — and
// says it is a hold so the caller can revoke an in-flight task on it.
func TestIssueAssignBlockReason_Hold(t *testing.T) {
	for _, label := range []string{"hold", "do-not-merge/hold", "paused-by-operator"} {
		t.Run(label, func(t *testing.T) {
			srv := assignGateServer(t, fmt.Sprintf(`{"number":42,"state":"open","labels":[{"name":%q}]}`, label))
			c := issueTestClient(t, srv.URL)
			c.SetHoldLabels([]string{"paused-by-operator"})
			gate, err := c.IssueAssignBlockReason(context.Background(), "o/r", 42)
			if err != nil {
				t.Fatal(err)
			}
			if gate.Reason != ClaimBlockReasonHold || !gate.Held || gate.AwaitingTriage {
				t.Fatalf("gate = %+v, want a hold refusal", gate)
			}
			if len(gate.Labels) != 1 || gate.Labels[0] != label {
				t.Fatalf("labels = %v, want the live labels", gate.Labels)
			}
		})
	}
}

// An untrusted (CONTRIBUTOR) reporter's issue with no triage label is
// refused under reporter trust; the same issue with the configured triage
// label, or filed by a trusted association, is admitted.
func TestIssueAssignBlockReason_ReporterTriage(t *testing.T) {
	for _, tc := range []struct {
		name, association, labels string
		wantBlocked               bool
	}{
		{"contributor untriaged", "CONTRIBUTOR", `[]`, true},
		{"contributor triaged", "CONTRIBUTOR", `[{"name":"triage/accepted"}]`, false},
		{"member untriaged", "MEMBER", `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := assignGateServer(t, fmt.Sprintf(
				`{"number":42,"state":"open","user":{"login":"someone","type":"User"},"author_association":%q,"labels":%s}`,
				tc.association, tc.labels))
			c := issueTestClient(t, srv.URL)
			c.SetIssueFilter(enabledReporterTrust())
			gate, err := c.IssueAssignBlockReason(context.Background(), "o/r", 42)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantBlocked {
				if !gate.AwaitingTriage || !strings.HasPrefix(gate.Reason, ClaimBlockReasonAwaitingTriagePrefix) ||
					!strings.Contains(gate.Reason, "triage/accepted") {
					t.Fatalf("gate = %+v, want an awaiting-triage refusal naming the label", gate)
				}
				return
			}
			if gate.Reason != "" || gate.AwaitingTriage {
				t.Fatalf("gate = %+v, want admitted", gate)
			}
		})
	}
}

// With reporter trust off (the default) an untriaged stranger's issue is
// not refused by this gate: nothing changes for hives that never opted in.
func TestIssueAssignBlockReason_ReporterTrustOffAdmits(t *testing.T) {
	srv := assignGateServer(t, `{"number":42,"state":"open","user":{"login":"someone","type":"User"},"author_association":"NONE","labels":[]}`)
	gate, err := issueTestClient(t, srv.URL).IssueAssignBlockReason(context.Background(), "o/r", 42)
	if err != nil || gate.Reason != "" {
		t.Fatalf("gate = %+v, %v; want admitted", gate, err)
	}
}

func TestIssueAssignBlockReason_Closed(t *testing.T) {
	srv := assignGateServer(t, `{"number":42,"state":"closed","labels":[]}`)
	gate, err := issueTestClient(t, srv.URL).IssueAssignBlockReason(context.Background(), "o/r", 42)
	if err != nil || gate.Reason != ClaimBlockReasonIssueClosed || !gate.Closed {
		t.Fatalf("gate = %+v, %v; want closed", gate, err)
	}
}

// A failed read is an error, never an admission.
func TestIssueAssignBlockReason_LookupErrorReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	gate, err := issueTestClient(t, srv.URL).IssueAssignBlockReason(context.Background(), "o/r", 42)
	if err == nil {
		t.Fatalf("gate = %+v with no error; a failed read must not admit", gate)
	}
}

// A fresh dispatch (the ledger's contributor and agent claims) applies the
// same reporter-trust gate.
func TestIssueContributorDispatchBlockReason_ReporterTriage(t *testing.T) {
	srv := assignGateServer(t, `{"number":42,"state":"open","user":{"login":"someone","type":"User"},"author_association":"CONTRIBUTOR","labels":[]}`)
	c := issueTestClient(t, srv.URL)
	c.SetIssueFilter(enabledReporterTrust())
	reason, err := c.IssueContributorDispatchBlockReason(context.Background(), "o/r", 42)
	if err != nil || !strings.HasPrefix(reason, ClaimBlockReasonAwaitingTriagePrefix) {
		t.Fatalf("reason = %q, %v; want awaiting triage", reason, err)
	}
}
