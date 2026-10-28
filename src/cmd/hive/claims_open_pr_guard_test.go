package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#14804: #14763 already had open PR #14771 ("Fixes #14763")
// when it was claimed and dispatched again, because the claim ledger had not
// yet scanned that PR. Ledger admission now checks open PRs live: a claim
// that would start new work is refused, while the claimant's own PR start
// signal, a renewal and the cleanup recheck of an existing claim are not.
func TestClaimAdmissionRefusesIssueWithOpenPR(t *testing.T) {
	for _, tc := range []struct {
		name       string
		guardOn    bool
		wantReason string
	}{
		{name: "guard on", guardOn: true, wantReason: github.ClaimBlockReasonOpenPRPrefix + "14771"},
		{name: "guard off", guardOn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
					_, _ = io.WriteString(w, `[{"number":14771,"state":"open","title":"fix","body":"Fixes #1","user":{"login":"alice"}}]`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/1":
					_, _ = io.WriteString(w, `{"number":1,"labels":[{"name":"bug"}]}`)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
					_, _ = io.WriteString(w, `{"name":"has-open-pr"}`)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/1/labels":
					_, _ = io.WriteString(w, `[]`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer api.Close()
			client := github.NewClientForTest(api.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			client.SetSkipIssuesWithOpenPR(tc.guardOn)
			ledger := testClaimsLedger(t)
			installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })

			req := claims.Request{Repo: "o/r", Issue: 1, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent}
			res, err := ledger.Claim(req)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantReason == "" {
				if res.Outcome != claims.OutcomeClaimed {
					t.Fatalf("claim with the guard off = %+v; want claimed", res)
				}
				return
			}
			if res.Outcome != claims.OutcomeRefused || res.Reason != tc.wantReason {
				t.Fatalf("new claim = %+v; want refused with %q", res, tc.wantReason)
			}

			// The claimant's own PR start signal is its evidence, not a duplicate.
			started := req
			started.PRStarted = true
			if res, err := ledger.Claim(started); err != nil || res.Outcome != claims.OutcomeClaimed {
				t.Fatalf("PR start signal = %+v, %v; want claimed", res, err)
			}
			// Renewing that claim, and the cleanup recheck of it, keep it.
			if res, err := ledger.Claim(req); err != nil || res.Outcome != claims.OutcomeRenewed {
				t.Fatalf("renewal = %+v, %v; want renewed", res, err)
			}
			if n := ledger.ReleaseBlocked(); n != 0 {
				t.Fatalf("cleanup tick released %d claims on the holder's own open PR", n)
			}
			// A different agent starting new work on it is refused.
			other := claims.Request{Repo: "o/r", Issue: 1, Holder: "fixer", HolderID: "fixer", Kind: claims.KindAgent, Force: true}
			if res, err := ledger.Claim(other); err != nil || res.Outcome != claims.OutcomeRefused || res.Reason != tc.wantReason {
				t.Fatalf("second agent claim = %+v, %v; want refused with %q", res, err, tc.wantReason)
			}
		})
	}
}
