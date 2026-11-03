package github

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

// The merge-request relay consults the contributor-lane policy gate before
// merging (hivecommons/docs#401: an untrusted-tier PR was merged via a
// scanner's merge request because the relay checked none of the lane gates).

func runGatedMergeRequest(t *testing.T, gate MergeRequestPolicyGate, merges *int) (*Client, string, []AuditRecord) {
	t.Helper()
	srv := newMergeMockServer(t, 0, merges)
	t.Cleanup(srv.Close)
	c := testMergeClient(t, srv.URL)
	c.SetMergeRequestPolicyGate(gate)

	var mu sync.Mutex
	var audits []AuditRecord
	c.SetAttributionAuditRecord(func(rec AuditRecord) {
		mu.Lock()
		defer mu.Unlock()
		audits = append(audits, rec)
	})

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	t.Cleanup(func() { mergeRequestDirForTest = "" })
	reqPath, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 42, Agent: "scanner", ExpectSHA: "abc", UpdateBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	return c, reqPath, append([]AuditRecord(nil), audits...)
}

func TestMergeRequestPolicyGateRefusalDeniesAndAudits(t *testing.T) {
	const refusal = "contributor-lane policy refuses the merge relay for @karibhou's PR: contributor-lane:claim-tier-untrusted (author tier \"untrusted\", require_tiers merger, trusted)"
	merges := 0
	var seen struct {
		repo   string
		number int
		sha    string
	}
	_, reqPath, audits := runGatedMergeRequest(t, func(_ context.Context, repo string, number int, expectSHA string) (string, error) {
		seen.repo, seen.number, seen.sha = repo, number, expectSHA
		return refusal, nil
	}, &merges)

	if merges != 0 {
		t.Fatalf("merges = %d, want 0: a refused request must never reach PUT /merge", merges)
	}
	if seen.repo != "o/r" || seen.number != 42 || seen.sha != "abc" {
		t.Fatalf("gate saw %+v, want o/r#42@abc", seen)
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || !strings.Contains(resp.Error, refusal) {
		t.Fatalf("result = %+v, want the policy refusal surfaced to the requester", resp)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Fatalf("request should be terminally refused (.denied): %v", err)
	}
	var found bool
	for _, rec := range audits {
		if rec.Action != AuditActionMergeRequestRefused {
			continue
		}
		found = true
		if rec.Agent != "scanner" || rec.Repo != "o/r" || rec.Target != 42 || !strings.Contains(rec.Detail, "contributor-lane:claim-tier-untrusted") || !strings.Contains(rec.Detail, "sha=abc") {
			t.Fatalf("audit record = %+v, want scanner o/r#42 with the policy reason", rec)
		}
	}
	if !found {
		t.Fatalf("no %s audit record among %+v", AuditActionMergeRequestRefused, audits)
	}
}

func TestMergeRequestPolicyGateErrorRetriesWithoutMerging(t *testing.T) {
	merges := 0
	calls := 0
	c, reqPath, audits := runGatedMergeRequest(t, func(context.Context, string, int, string) (string, error) {
		calls++
		return "", errors.New("lane evidence unavailable")
	}, &merges)

	if merges != 0 {
		t.Fatalf("merges = %d, want 0: the gate fails closed", merges)
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("a gate error is retryable; the request should remain live: %v", err)
	}
	for _, rec := range audits {
		if rec.Action == AuditActionMergeRequestRefused {
			t.Fatalf("a transient gate error must not be audited as a refusal: %+v", rec)
		}
	}
	// The retry consults the gate again; once it clears, the merge proceeds.
	c.SetMergeRequestPolicyGate(func(context.Context, string, int, string) (string, error) { return "", nil })
	c.ProcessMergeRequestsOnce(context.Background())
	if merges != 1 || calls != 1 {
		t.Fatalf("merges = %d calls = %d, want 1/1 after the gate clears", merges, calls)
	}
}

func TestMergeRequestPolicyGateAllowsAndNilGateKeepsBehaviour(t *testing.T) {
	t.Run("gate allows", func(t *testing.T) {
		merges := 0
		_, reqPath, audits := runGatedMergeRequest(t, func(context.Context, string, int, string) (string, error) { return "", nil }, &merges)
		if merges != 1 {
			t.Fatalf("merges = %d, want 1", merges)
		}
		if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
			t.Fatalf("successful merge should consume the request file: %v", err)
		}
		for _, rec := range audits {
			if rec.Action == AuditActionMergeRequestRefused {
				t.Fatalf("unexpected refusal audit: %+v", rec)
			}
		}
	})
	t.Run("nil gate", func(t *testing.T) {
		merges := 0
		_, _, _ = runGatedMergeRequest(t, nil, &merges)
		if merges != 1 {
			t.Fatalf("merges = %d, want 1 without a gate", merges)
		}
	})
	t.Run("nil client setter is safe", func(t *testing.T) {
		var c *Client
		c.SetMergeRequestPolicyGate(func(context.Context, string, int, string) (string, error) { return "", nil })
	})
}
