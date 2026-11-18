package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestInvalidPRHeadReason(t *testing.T) {
	for _, tc := range []struct {
		head    string
		invalid bool
	}{
		{"HEAD", true},
		{" HEAD ", true},
		{"refs/heads/HEAD", true},
		{"@", true},
		{"hive/sec-check/20261118-2203", false},
		{"scanner/fix-18907", false},
		{"head", false}, // a real (if unwise) branch name
	} {
		if got := invalidPRHeadReason(tc.head) != ""; got != tc.invalid {
			t.Errorf("invalidPRHeadReason(%q) invalid=%v, want %v", tc.head, got, tc.invalid)
		}
	}
}

func headInvalid422() error {
	return &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
		Message:  "Validation Failed",
		Errors:   []gh.Error{{Resource: "PullRequest", Field: "head", Code: "invalid"}},
	}
}

func TestPRHeadInvalidClassifiesOnly422HeadInvalid(t *testing.T) {
	if !prHeadInvalid(fmt.Errorf("creating PR o/r HEAD->main: %w", headInvalid422())) {
		t.Fatal("wrapped 422 head invalid not recognised")
	}
	other422 := &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
		Errors:   []gh.Error{{Resource: "PullRequest", Field: "base", Code: "invalid"}},
	}
	for _, err := range []error{nil, errors.New("temporary failure"), other422} {
		if prHeadInvalid(err) {
			t.Errorf("prHeadInvalid(%v) = true, want false", err)
		}
	}
}

// #18907: a 422 head invalid is terminal after a bounded number of attempts,
// is recorded in the audit log, and is not retried to the 24h horizon.
func TestFailPRRequestEndsHeadInvalidAfterBoundedAttempts(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:0")
	var audits []auditRec
	c.SetAttributionHooks(AttributionHooks{
		Audit: func(action, detail, agent string) { audits = append(audits, auditRec{action, detail, agent}) },
	})
	dir := t.TempDir()
	path, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "sec-check/fix", Title: "fix", Body: "Fixes #1", Agent: "sec-check"})
	if err != nil {
		t.Fatal(err)
	}
	req := PRRequest{Repo: "o/r", Head: "sec-check/fix", Agent: "sec-check"}
	now := time.Now()
	for i := 1; i <= prHeadInvalidMaxAttempts; i++ {
		c.failPRRequest(path, req, headInvalid422(), func() time.Time { return now.Add(time.Duration(i) * time.Hour) })
		_, statErr := os.Stat(path)
		if i < prHeadInvalidMaxAttempts && statErr != nil {
			t.Fatalf("attempt %d: request no longer queued before the bound: %v", i, statErr)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("request still queued after %d head-invalid attempts", prHeadInvalidMaxAttempts)
	}
	if _, err := os.Stat(path + ".rejected"); err != nil {
		t.Fatalf("request not quarantined as rejected: %v", err)
	}
	res, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".result.json")
	if err != nil || !strings.Contains(string(res), "PR head rejected") {
		t.Fatalf("result does not explain the head rejection: %s (%v)", res, err)
	}
	if c.prRetries.attempts(path) != 0 {
		t.Fatal("retry state not cleared for the ended request")
	}
	if len(audits) != 1 || audits[0].action != AuditActionAgentPRRejected || audits[0].agent != "sec-check" {
		t.Fatalf("audits = %+v, want one %s for sec-check", audits, AuditActionAgentPRRejected)
	}
	for _, frag := range []string{"repo=o/r", "head=sec-check/fix", "attempts=3"} {
		if !strings.Contains(audits[0].detail, frag) {
			t.Errorf("audit detail missing %q: %q", frag, audits[0].detail)
		}
	}
}

// #18907: a request carrying the literal head "HEAD" is refused once, before
// any GitHub call, instead of reaching a 422 it would retry all day.
func TestPRRequestWatcher_RefusesDetachedHeadBeforeGitHub(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	var audits []auditRec
	c.SetAttributionHooks(AttributionHooks{
		Audit: func(action, detail, agent string) { audits = append(audits, auditRec{action, detail, agent}) },
	})

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()
	path, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "HEAD", Title: "fix", Body: "Fixes #1", Agent: "sec-check"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if n := calls.Load(); n != 0 {
		t.Fatalf("watcher made %d GitHub calls for a HEAD head, want 0", n)
	}
	if _, err := os.Stat(path + ".rejected"); err != nil {
		t.Fatalf("HEAD request not rejected: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.result.json"))
	if len(matches) != 1 {
		t.Fatalf("result files = %v, want 1", matches)
	}
	res, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(res), "detached HEAD") {
		t.Fatalf("result does not name the detached HEAD: %s", res)
	}
	if len(audits) != 1 || audits[0].action != AuditActionAgentPRRejected {
		t.Fatalf("audits = %+v, want one %s", audits, AuditActionAgentPRRejected)
	}
}
