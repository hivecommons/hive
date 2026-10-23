package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// The label is borrowed, never minted: a hive reviews other people's repos, so
// an unknown name must be a no-op rather than a new label in their taxonomy.
func TestApplyHumanDecisionLabel_AppliesExistingLabel(t *testing.T) {
	var applied []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/labels/queue-triage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"queue-triage"}`))
	})
	mux.HandleFunc("/repos/acme/widget/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
		applied = append(applied, r.Method)
		_, _ = w.Write([]byte(`[{"name":"queue-triage"}]`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	if err := c.ApplyHumanDecisionLabel(context.Background(), "acme/widget", 42, "queue-triage"); err != nil {
		t.Fatalf("expected label to apply, got %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("expected exactly one add-labels call, got %d", len(applied))
	}
}

func TestApplyHumanDecisionLabel_MissingLabelIsNotCreated(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/labels/typo-queue", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	mux.HandleFunc("/repos/acme/widget/labels", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("must not create a label the repo does not define")
	})
	mux.HandleFunc("/repos/acme/widget/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("must not apply a label that does not exist")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	err := c.ApplyHumanDecisionLabel(context.Background(), "acme/widget", 42, "typo-queue")
	if !errors.Is(err, ErrHumanDecisionLabelMissing) {
		t.Fatalf("expected a missing-label notice, got %v", err)
	}
	var apiErr *gh.ErrorResponse
	if !errors.As(err, &apiErr) || apiErr.Response.StatusCode != http.StatusNotFound {
		t.Fatalf("underlying GitHub 404 must remain available, got %v", err)
	}
}

func TestApplyHumanDecisionLabel_APIFailuresAreNotMissingLabels(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lookupStatus int
		writeStatus  int
	}{
		{"lookup forbidden", http.StatusForbidden, 0},
		{"lookup unavailable", http.StatusServiceUnavailable, 0},
		{"write not found", http.StatusOK, http.StatusNotFound},
		{"write forbidden", http.StatusOK, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /repos/acme/widget/labels/queue-triage":
					w.WriteHeader(tc.lookupStatus)
				case "POST /repos/acme/widget/issues/42/labels":
					if tc.writeStatus == 0 {
						t.Error("must not write after a failed lookup")
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(tc.writeStatus)
				default:
					t.Errorf("unexpected API call %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			c := newTestClient(t, server, "acme", []string{"widget"})
			err := c.ApplyHumanDecisionLabel(context.Background(), "acme/widget", 42, "queue-triage")
			if err == nil || errors.Is(err, ErrHumanDecisionLabelMissing) {
				t.Fatalf("API failure must remain an error, not a missing-label notice: %v", err)
			}
			wantStatus := tc.lookupStatus
			if tc.writeStatus != 0 {
				wantStatus = tc.writeStatus
			}
			var apiErr *gh.ErrorResponse
			if !errors.As(err, &apiErr) || apiErr.Response.StatusCode != wantStatus {
				t.Fatalf("want underlying GitHub status %d, got %v", wantStatus, err)
			}
		})
	}
}

// An unset label is the default posture, not a misconfiguration: the review
// marker alone is a complete signal, so this must not reach the API at all.
func TestApplyHumanDecisionLabel_EmptyIsSilentNoOp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call %s %s for an unset label", r.Method, r.URL.Path)
	}))
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	if err := c.ApplyHumanDecisionLabel(context.Background(), "acme/widget", 42, "  "); err != nil {
		t.Fatalf("unset label must be a silent no-op, got %v", err)
	}
}
