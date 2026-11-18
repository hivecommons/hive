package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

func TestKickClaimsRespectNeedsHumanAndReleaseOnTick(t *testing.T) {
	var label atomic.Value
	label.Store("needs-human")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"number":1,"labels":[{"name":%q}]}`, label.Load())
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	ledger := testClaimsLedger(t)
	installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })
	srv := dashboard.NewServer(0, logger)
	srv.RegisterAPI(&dashboard.Dependencies{Config: &config.Config{}, IssueClaims: ledger})
	t.Cleanup(srv.CloseContributeHub)

	recordAgentKickListings(srv, "o", "scanner", []string{"r#1"}, logger)
	if _, listed := ledger.Listed("o/r", 1); !listed {
		t.Fatal("kick did not record listing")
	}
	recordAgentStart(ledger, "o", "scanner", "r", 1, "comment", logger)
	if _, ok := ledger.Lookup("o/r", 1); ok {
		t.Fatal("needs-human issue claimed on start")
	}
	label.Store("bug")
	recordAgentStart(ledger, "o", "scanner", "r", 1, "comment", logger)
	if c, ok := ledger.Lookup("o/r", 1); !ok || c.Kind != claims.KindAgent {
		t.Fatal("ordinary issue not claimed")
	}
	label.Store("needs-human")
	if n := ledger.ReleaseBlocked(); n != 1 {
		t.Fatalf("cleanup tick released %d claims, want 1", n)
	}
	if _, held := claimsInflightLookup(ledger, "o")(github.Issue{Repo: "r", Number: 1}); held {
		t.Fatal("released claim still reported In Flight")
	}
}

// hivecommons/hive#12994: a review-swarm fix kick listed the PR in the issue
// ledger, the author's next comment on its own PR was taken as an issue start
// signal, and the resulting agent claim staged the PR
// stage/030-pr-in-progress — an issue-only stage. Even when a PR number is
// listed, admission refuses the claim: no hook fires, no label is written.
func TestKickClaimsRefusePullRequests(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":12994,"labels":[{"name":"size/L"}],"pull_request":{"url":"https://api.github.example/repos/o/r/pulls/12994"}}`)
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	var claimed atomic.Int32
	ledger, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{
		OnClaimed: func(claims.Claim, claims.Outcome) { claimed.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })

	ledger.MarkListed("o/r", 12994, "scanner")
	recordAgentStart(ledger, "o", "scanner", "r", 12994, github.AgentStartSignalComment, logger)
	if c, ok := ledger.Lookup("o/r", 12994); ok {
		t.Fatalf("pull request claimed as an issue on start: %+v", c)
	}
	if n := claimed.Load(); n != 0 {
		t.Fatalf("OnClaimed fired %d times for a pull request", n)
	}
	res, err := ledger.Claim(claims.Request{Repo: "o/r", Issue: 12994, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent})
	if err != nil || res.Outcome != claims.OutcomeRefused || res.Reason != github.ClaimBlockReasonPullRequest {
		t.Fatalf("direct claim on a pull request = %+v, %v; want refused with %q", res, err, github.ClaimBlockReasonPullRequest)
	}
}

// hivecommons/hive#12999 (label Epic) and #13011 (parked, then gated by the
// maintainer) were claimed by the scanner on a kick listing. Admission now
// refuses an umbrella issue and a parked issue for every producer: the start
// signal records nothing, no hook fires, a direct claim is refused with the
// reason, and a live claim on one is released on the cleanup tick.
func TestKickClaimsRefuseEpicsAndParkedIssues(t *testing.T) {
	for _, tc := range []struct {
		label      string
		wantReason string
	}{
		{"Epic", github.ClaimBlockReasonNeverClaimPrefix + "Epic"},
		{"stage/045-needs-decision", github.ClaimBlockReasonParkedPrefix + "stage/045-needs-decision"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var label atomic.Value
			label.Store(tc.label)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"number":12999,"labels":[{"name":%q},{"name":"help wanted"}]}`, label.Load())
			}))
			defer api.Close()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
			var claimed atomic.Int32
			ledger, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{
				OnClaimed: func(claims.Claim, claims.Outcome) { claimed.Add(1) },
			})
			if err != nil {
				t.Fatal(err)
			}
			installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })

			ledger.MarkListed("o/r", 12999, "scanner")
			recordAgentStart(ledger, "o", "scanner", "r", 12999, github.AgentStartSignalClaim, logger)
			if c, ok := ledger.Lookup("o/r", 12999); ok {
				t.Fatalf("%s claimed on start: %+v", tc.label, c)
			}
			if n := claimed.Load(); n != 0 {
				t.Fatalf("OnClaimed fired %d times", n)
			}
			res, err := ledger.Claim(claims.Request{Repo: "o/r", Issue: 12999, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent})
			if err != nil || res.Outcome != claims.OutcomeRefused || res.Reason != tc.wantReason {
				t.Fatalf("direct claim = %+v, %v; want refused with %q", res, err, tc.wantReason)
			}

			// A claim recorded before the label arrived is released on the tick.
			label.Store("bug")
			recordAgentStart(ledger, "o", "scanner", "r", 12999, github.AgentStartSignalClaim, logger)
			if _, ok := ledger.Lookup("o/r", 12999); !ok {
				t.Fatal("ordinary issue not claimed")
			}
			label.Store(tc.label)
			if n := ledger.ReleaseBlocked(); n != 1 {
				t.Fatalf("cleanup tick released %d claims, want 1", n)
			}
		})
	}
}

// hivecommons/hive#14799: #14766 was dispatched to a relay contributor six
// minutes after a person's PR fixing it opened, because admission read only
// the eval cycle's claim ledger. A fresh automated claim now asks the forge
// for an open PR linking the issue and is refused; a PR-backed start signal,
// a renewal and the release tick's recheck do not consult the listing, so
// the agent whose own PR links the issue keeps its claim.
func TestKickClaimsRefuseIssueWithOpenLinkedPR(t *testing.T) {
	var pullsCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/pulls":
			pullsCalls.Add(1)
			fmt.Fprint(w, `[{"number":14775,"state":"open","title":"fix","body":"Fixes #14766","html_url":"https://github.com/o/r/pull/14775","created_at":"2026-10-27T01:00:00Z","user":{"login":"clubanderson","type":"User"}}]`)
		case r.URL.Path == "/repos/o/r/issues/14766":
			fmt.Fprint(w, `{"number":14766,"labels":[{"name":"bug"}]}`)
		case r.URL.Path == "/repos/o/r/issues/14766/labels", r.URL.Path == "/repos/o/r/labels":
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	ledger := testClaimsLedger(t)
	installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })

	res, err := ledger.Claim(claims.Request{Repo: "o/r", Issue: 14766, Holder: "relay", HolderID: "c-relay", Kind: claims.KindContributor})
	if err != nil || res.Outcome != claims.OutcomeRefused || res.Reason != github.ClaimBlockReasonOpenPRPrefix+"14775" {
		t.Fatalf("fresh claim = %+v, %v; want refused by the open PR", res, err)
	}
	if pullsCalls.Load() != 1 {
		t.Fatalf("open-PR listings = %d, want 1", pullsCalls.Load())
	}

	// The PR author's own agent start signal carries PR evidence: admitted.
	res, err = ledger.Claim(claims.Request{Repo: "o/r", Issue: 14766, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent, PRStarted: true})
	if err != nil || res.Outcome != claims.OutcomeClaimed {
		t.Fatalf("PR-backed claim = %+v, %v; want claimed", res, err)
	}
	// Renewal and the release tick recheck the held claim without listing.
	res, err = ledger.Claim(claims.Request{Repo: "o/r", Issue: 14766, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent})
	if err != nil || res.Outcome != claims.OutcomeRenewed {
		t.Fatalf("renewal = %+v, %v; want renewed", res, err)
	}
	if n := ledger.ReleaseBlocked(); n != 0 {
		t.Fatalf("release tick dropped %d claims, want 0: the holder's own PR is the open PR", n)
	}
	if _, ok := ledger.Lookup("o/r", 14766); !ok {
		t.Fatal("agent claim must survive")
	}
	if pullsCalls.Load() != 1 {
		t.Fatalf("open-PR listings = %d, want still 1 (renewal/tick never list)", pullsCalls.Load())
	}
}

// hivecommons/hive#17299: an issue an agent filed without --claim is filed
// for outside help, and the hub's own scanner dispatch of it back to that
// agent (#17243→#17250) is refused — not only the agent's relay `claim`
// request. Another agent still takes it.
func TestKickClaimsRefuseFilerOfFiledForHelpIssue(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/issues/17243":
			body, _ := json.Marshal(map[string]any{
				"number": 17243, "state": "open", "labels": []map[string]string{{"name": "help wanted"}},
				"body": "someone take this\n\n" + github.FiledForHelpMarker("scanner"),
			})
			_, _ = w.Write(body)
		case r.URL.Path == "/repos/o/r/pulls", r.URL.Path == "/repos/o/r/issues/17243/labels", r.URL.Path == "/repos/o/r/labels":
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	ledger := testClaimsLedger(t)
	installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })

	res, err := ledger.Claim(claims.Request{Repo: "o/r", Issue: 17243, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent})
	if err != nil || res.Outcome != claims.OutcomeRefused || !strings.HasPrefix(res.Reason, github.ClaimBlockReasonFiledForHelpPrefix+"scanner") {
		t.Fatalf("filer's dispatch = %+v, %v; want refused with %q", res, err, github.ClaimBlockReasonFiledForHelpPrefix+"scanner")
	}
	ledger.MarkListed("o/r", 17243, "scanner")
	recordAgentStart(ledger, "o", "scanner", "r", 17243, github.AgentStartSignalClaim, logger)
	if c, ok := ledger.Lookup("o/r", 17243); ok {
		t.Fatalf("filer's start signal claimed its own filed-for-help issue: %+v", c)
	}

	res, err = ledger.Claim(claims.Request{Repo: "o/r", Issue: 17243, Holder: "reviewer", HolderID: "reviewer", Kind: claims.KindAgent})
	if err != nil || res.Outcome != claims.OutcomeClaimed {
		t.Fatalf("another agent's claim = %+v, %v; want claimed", res, err)
	}
}

// hivecommons/hive#17299: an issue an agent files with --claim was never in
// a kick, so a plain `claim` start signal on it ledgers nothing and every
// ledger-only reader (dispatch admission Lookup, the PR-request gate, the
// claim listing) still sees it unclaimed. The filed-claim signal the create
// path raises is the issue's own listing: the ledger claim is recorded, the
// admission check still runs, and another agent's dispatch is refused.
func TestRecordAgentStartFiledClaimLedgersSelfFiledIssue(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/issues/17243":
			body, _ := json.Marshal(map[string]any{
				"number": 17243, "state": "open", "body": "stage 2 of my epic",
				"labels": []map[string]string{{"name": "hive/claimed-by-scanner"}},
			})
			_, _ = w.Write(body)
		case r.URL.Path == "/repos/o/r/pulls", r.URL.Path == "/repos/o/r/issues/17243/labels", r.URL.Path == "/repos/o/r/labels":
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	ledger := testClaimsLedger(t)
	installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })

	// The plain claim signal a `claim` request raises: no kick listed the
	// issue, so the ledger stays empty — the gap the filed-claim signal closes.
	recordAgentStart(ledger, "o", "scanner", "r", 17243, github.AgentStartSignalClaim, logger)
	if c, ok := ledger.Lookup("o/r", 17243); ok {
		t.Fatalf("plain claim signal on an unlisted issue recorded a claim: %+v", c)
	}

	recordAgentStart(ledger, "o", "scanner", "r", 17243, github.AgentStartSignalFiledClaim, logger)
	c, ok := ledger.Lookup("o/r", 17243)
	if !ok || c.Holder != "scanner" || c.Kind != claims.KindAgent || c.PRStarted {
		t.Fatalf("filed-claim signal did not ledger the self-filed issue to its filer: %+v, %v", c, ok)
	}
	if li, listed := ledger.Listed("o/r", 17243); listed {
		t.Fatalf("listing outlived the claim it became: %+v", li)
	}

	res, err := ledger.Claim(claims.Request{Repo: "o/r", Issue: 17243, Holder: "reviewer", HolderID: "reviewer", Kind: claims.KindAgent})
	if err != nil || res.Outcome != claims.OutcomeHeld || res.Claim.Holder != "scanner" {
		t.Fatalf("another agent's dispatch of the self-filed issue = %+v, %v; want held by scanner", res, err)
	}
}

// hivecommons/hive#18898: a contributor's fresh claim on an issue an owner
// filed moments ago is refused with the owner-grace reason; the same issue
// after the window, or a hive agent's own claim on it, is admitted.
func TestContributorClaimRefusedInsideOwnerGrace(t *testing.T) {
	var ageMin atomic.Int64
	ageMin.Store(2)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues/7" {
			http.NotFound(w, r)
			return
		}
		created := time.Now().Add(-time.Duration(ageMin.Load()) * time.Minute).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"number":7,"state":"open","created_at":%q,"author_association":"OWNER","user":{"login":"andy"},"labels":[]}`, created)
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	client.SetContributorGrace(15 * time.Minute)
	ledger := testClaimsLedger(t)
	installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })

	req := claims.Request{Repo: "o/r", Issue: 7, Holder: "karibhou", HolderID: "c-1", Kind: claims.KindContributor}
	res, err := ledger.Claim(req)
	if err != nil || res.Outcome != claims.OutcomeRefused || !strings.HasPrefix(res.Reason, github.ClaimBlockReasonOwnerGracePrefix) {
		t.Fatalf("claim inside the grace = %+v, %v; want refused with the owner grace reason", res, err)
	}
	if _, held := ledger.Lookup("o/r", 7); held {
		t.Fatal("a refused contributor claim must not be recorded")
	}
	ageMin.Store(30)
	res, err = ledger.Claim(req)
	if err != nil || res.Outcome != claims.OutcomeClaimed {
		t.Fatalf("claim after the grace = %+v, %v; want claimed", res, err)
	}
}

// hivecommons/hive#18899: the operator's dispatcher claims with an assignee
// plus a plain comment and no ledger entry. A contributor's live lease is
// released on the release tick with the human-claim reason, a renewal and a
// fresh claim are refused, and the contributor's own assignment is not a claim
// against it.
func TestContributorLeaseReleasedByOutOfLedgerHumanClaim(t *testing.T) {
	var assigned atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/9":
			if assigned.Load() {
				fmt.Fprint(w, `{"number":9,"state":"open","assignees":[{"login":"andy"}],"labels":[]}`)
				return
			}
			fmt.Fprint(w, `{"number":9,"state":"open","assignees":[],"labels":[]}`)
		case "/repos/o/r/issues/9/timeline":
			fmt.Fprint(w, `[{"event":"assigned","assignee":{"login":"andy"},"actor":{"login":"andy"}}]`)
		case "/repos/o/r/issues/9/comments":
			if assigned.Load() {
				fmt.Fprint(w, `[{"user":{"login":"andy"},"author_association":"MEMBER","body":"Claimed; fix in progress."}]`)
				return
			}
			fmt.Fprint(w, `[]`)
		case "/repos/o/r/collaborators/andy/permission":
			fmt.Fprint(w, `{"permission":"admin","role_name":"admin","user":{"login":"andy"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	var released atomic.Value
	ledger, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{
		OnReleased: func(c claims.Claim, reason string) { released.Store(reason) },
	})
	if err != nil {
		t.Fatal(err)
	}
	installClaimAdmissionCheck(context.Background(), ledger, func() *github.Client { return client })

	req := claims.Request{Repo: "o/r", Issue: 9, Holder: "karibhou", HolderID: "c-1", Kind: claims.KindContributor}
	if res, err := ledger.Claim(req); err != nil || res.Outcome != claims.OutcomeClaimed {
		t.Fatalf("claim before the owner acts = %+v, %v; want claimed", res, err)
	}
	if n := ledger.ReleaseBlocked(); n != 0 {
		t.Fatalf("released %d claims with no human claim on the forge", n)
	}

	// The owner assigns the issue and posts "Claimed; fix in progress.".
	assigned.Store(true)
	// The verdict cache would still answer "no claim" for a minute in
	// production; this client has not asked since, but use a fresh one so the
	// test does not depend on that window.
	client = github.NewClientForTest(api.URL, "o", []string{"r"}, logger)
	res, err := ledger.Claim(req)
	if err != nil || res.Outcome != claims.OutcomeRefused {
		t.Fatalf("renewal after the owner claimed = %+v, %v; want refused", res, err)
	}
	if login, ok := claims.ParseHumanClaimReason(res.Reason); !ok || login != "andy" {
		t.Fatalf("renewal refusal reason = %q, want a human-claim reason naming andy", res.Reason)
	}
	if n := ledger.ReleaseBlocked(); n != 1 {
		t.Fatalf("release tick released %d claims, want 1", n)
	}
	if reason, _ := released.Load().(string); !strings.HasPrefix(reason, claims.ReasonHumanClaim) {
		t.Fatalf("release reason = %q, want the human-claim reason", reason)
	}
	if _, held := ledger.Lookup("o/r", 9); held {
		t.Fatal("contributor claim survived the release tick")
	}
	// A fresh contributor claim is refused while the person's claim stands.
	if res, err := ledger.Claim(claims.Request{Repo: "o/r", Issue: 9, Holder: "other", HolderID: "c-2", Kind: claims.KindContributor}); err != nil || res.Outcome != claims.OutcomeRefused {
		t.Fatalf("fresh contributor claim = %+v, %v; want refused", res, err)
	}
	// The person's own ledger claim is unaffected.
	if res, err := ledger.Claim(claims.Request{Repo: "o/r", Issue: 9, Holder: "andy", Kind: claims.KindHuman}); err != nil || res.Outcome != claims.OutcomeClaimed {
		t.Fatalf("human claim = %+v, %v; want claimed", res, err)
	}
}
