package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/issueclaim"
)

func claimValidationServer(t *testing.T, files []string, issue map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/compare/"):
			changed := make([]map[string]string, 0, len(files))
			for _, file := range files {
				changed = append(changed, map[string]string{"filename": file, "status": "modified"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "files": changed})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/"):
			_ = json.NewEncoder(w).Encode(issue)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func claimValidationClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestValidatePRRequestClaims_ArtifactTitleMatchesDiff(t *testing.T) {
	tests := []struct {
		name  string
		title string
		file  string
	}{
		{"workflow", "ci: add upstream sync workflow", ".github/workflows/sync.yml"},
		{"test", "test: cover retry behavior", "src/retry_test.go"},
		{"cross-language test", "test: cover sync helper", "build-aux/sync_test.py"},
		{"migration", "db: add user migration", "db/migrations/001_users.sql"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := claimValidationServer(t, []string{tt.file}, nil)
			defer srv.Close()
			c := claimValidationClient(t, srv)
			gotTitle, _, err := c.validatePRRequestClaims(context.Background(), PRRequest{
				Repo: "o/r", Base: "main", Head: "agent/change", Title: tt.title,
			})
			if err != nil {
				t.Fatalf("validatePRRequestClaims: %v", err)
			}
			if gotTitle != tt.title {
				t.Fatalf("title = %q, want %q", gotTitle, tt.title)
			}
		})
	}
}

func TestValidatePRRequestClaims_RejectsMissingClaimedArtifact(t *testing.T) {
	srv := claimValidationServer(t, []string{"docs/UPSTREAM.md", "build-aux/sync-upstream.sh"}, nil)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	_, _, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Base: "main", Head: "agent/change", Title: "ci: add upstream sync workflow",
	})
	reason, ok := prRequestPolicyReason(err)
	if !ok || reason != "title claims workflow but diff contains no workflow file" {
		t.Fatalf("error = %v (%q, policy=%v)", err, reason, ok)
	}
}

func TestValidatePRRequestClaims_PagesCompareFilesForClaimedArtifact(t *testing.T) {
	var srv *httptest.Server
	pages := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pages++
		names := make([]map[string]string, 0, githubCompareFileLimit)
		if r.URL.Query().Get("page") == "2" {
			names = append(names, map[string]string{"filename": ".github/workflows/ci.yml", "status": "added"})
		} else {
			w.Header().Set("Link", "<"+srv.URL+"/repos/o/r/compare/main...agent/change?per_page=300&page=2>; rel=\"next\"")
			for i := 0; i < githubCompareFileLimit; i++ {
				names = append(names, map[string]string{"filename": "docs/f.md", "status": "modified"})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "files": names})
	}))
	defer srv.Close()
	c := claimValidationClient(t, srv)

	_, _, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Base: "main", Head: "agent/change", Title: "ci: add workflow",
	})
	if err != nil {
		t.Fatalf("workflow on page 2 must satisfy the claim: %v", err)
	}
	if pages != 2 {
		t.Fatalf("pages requested = %d, want 2", pages)
	}
}

func TestPRRequestWatcher_QuarantinesArtifactClaimMismatch(t *testing.T) {
	created := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/compare/"):
			_, _ = io.WriteString(w, `{"status":"ahead","files":[{"filename":"docs/UPSTREAM.md"}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			created++
			_, _ = io.WriteString(w, `{"number":42}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := claimValidationClient(t, srv)
	c.prAuthz = func(string, int) error { return nil }

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()
	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Base: "main", Head: "agent/change", Title: "ci: add upstream sync workflow", Body: "adds the workflow", Agent: "scanner",
	})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("mismatched title created %d PRs, want 0", created)
	}
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Fatalf("rejected request was not quarantined: %v", err)
	}
	result, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), "title claims workflow but diff contains no workflow file") {
		t.Fatalf("result lacks actionable mismatch: %s", result)
	}
}

func TestValidatePRRequestClaims_DowngradesIncompleteIssues(t *testing.T) {
	tests := []struct {
		name       string
		issue      map[string]any
		wantReason string
	}{
		{
			"unchecked task delegating to another issue",
			map[string]any{"number": 60, "title": "work", "body": "- [x] #100 first\n- [ ] #101 remaining", "state": "open"},
			"issue has unchecked task items that delegate work to other issues",
		},
		{
			"epic label",
			map[string]any{"number": 60, "title": "work", "body": "several phases", "state": "open", "labels": []map[string]string{{"name": "epic"}}},
			"issue is labeled as a tracker or epic",
		},
		{
			"tracker title",
			map[string]any{"number": 60, "title": "[Tracker] program", "body": "children", "state": "open"},
			"issue title marks it as a tracker or epic",
		},
		{
			"epic title",
			map[string]any{"number": 60, "title": "[EPIC] program", "body": "children", "state": "open"},
			"issue title marks it as a tracker or epic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := claimValidationServer(t, nil, tt.issue)
			defer srv.Close()
			c := claimValidationClient(t, srv)
			title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
				Repo: "o/r", Head: "agent/change", Title: "Fixes #60: partial work", Body: "Closes: #60\n\nDetails",
			})
			if err != nil {
				t.Fatalf("validatePRRequestClaims: %v", err)
			}
			// The body names the withheld keyword and why (#7156); the title
			// is rewritten bare, since a note does not belong in a title.
			wantBody := "Refs: #60" + downgradeNote(tt.wantReason) + "\n\nDetails"
			if title != "Refs #60: partial work" || body != wantBody {
				t.Fatalf("downgraded title/body = %q / %q, want %q / %q",
					title, body, "Refs #60: partial work", wantBody)
			}
		})
	}
}

func TestValidatePRRequestClaims_LeavesCompleteIssueClosingReference(t *testing.T) {
	issue := map[string]any{"number": 7, "title": "focused bug", "body": "One concrete acceptance criterion", "state": "open"}
	srv := claimValidationServer(t, nil, issue)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "agent/change", Title: "fix: focused bug", Body: "Fixes #7",
	})
	if err != nil {
		t.Fatalf("validatePRRequestClaims: %v", err)
	}
	if title != "fix: focused bug" || body != "Fixes #7" {
		t.Fatalf("title/body changed: %q / %q", title, body)
	}
}

// TestValidatePRRequestClaims_KeepsHumanFiledBugClosingReference pins the
// scanner/contributor PR-body contract: a fix PR keeps its closing keyword, and
// the reporter-confirmation policy, when explicitly opted in, is enforced by
// the issue close path instead of by rewriting the PR to a non-closing Refs line.
func TestValidatePRRequestClaims_KeepsHumanFiledBugClosingReference(t *testing.T) {
	// A maintainer-filed bug: has the "bug" label, no hive attribution
	// trailer in the body, and User.Type is "User" (not "Bot").
	issue := map[string]any{
		"number": 6500,
		"title":  "Copilot license check false-fails",
		"body":   "The strategist agent claims no Copilot license. Steps to reproduce:\n1. Do X\n2. Y happens",
		"state":  "open",
		"labels": []map[string]string{{"name": "bug"}},
		"user":   map[string]any{"login": "MikeSpreitzer", "type": "User"},
	}
	srv := claimValidationServer(t, nil, issue)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "agent/fix", Title: "fix copilot check", Body: "Closes #6500\n\nDetails",
	})
	if err != nil {
		t.Fatalf("validatePRRequestClaims: %v", err)
	}
	if body != "Closes #6500\n\nDetails" {
		t.Fatalf("human-filed bug PR should keep its closing keyword; issue close gate handles any opt-in reporter confirmation: got %q", body)
	}
	if title != "fix copilot check" {
		t.Fatalf("title mutated unexpectedly: %q", title)
	}
}

// TestValidatePRRequestClaims_LeavesAgentFiledBugClosingReference confirms the
// gate is scoped: an agent's own bug-labeled finding still auto-closes on
// merge. Detection: the body carries AttributionTrailerPrefix.
func TestValidatePRRequestClaims_LeavesAgentFiledBugClosingReference(t *testing.T) {
	issue := map[string]any{
		"number": 6781,
		"title":  "[strategist] closed-unverified loop",
		"body":   "Details.\n\n— hive: agent=strategist backend=copilot model=claude-sonnet-4-6",
		"state":  "open",
		"labels": []map[string]string{{"name": "bug"}, {"name": "agent/strategist"}},
		"user":   map[string]any{"login": "kubestellar-hive", "type": "Bot"},
	}
	srv := claimValidationServer(t, nil, issue)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "agent/fix", Title: "fix loop", Body: "Closes #6781",
	})
	if err != nil {
		t.Fatalf("validatePRRequestClaims: %v", err)
	}
	if title != "fix loop" || body != "Closes #6781" {
		t.Fatalf("agent-filed bug should still be closeable: %q / %q", title, body)
	}
}

// TestValidatePRRequestClaims_HonoursReporterConfirmation confirms that a
// maintainer/reporter can opt in to auto-close by dropping the
// humanFiledBugConfirmationMarker in the body. PR claim validation now keeps
// closing keywords either way; the marker is consumed by the issue close gate.
func TestValidatePRRequestClaims_HonoursReporterConfirmation(t *testing.T) {
	issue := map[string]any{
		"number": 42,
		"title":  "reporter-confirmed bug",
		"body":   "Steps to reproduce ...\n\nhive: reporter-confirmed",
		"state":  "open",
		"labels": []map[string]string{{"name": "bug"}},
		"user":   map[string]any{"login": "some-maintainer", "type": "User"},
	}
	srv := claimValidationServer(t, nil, issue)
	defer srv.Close()
	c := claimValidationClient(t, srv)

	title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "agent/fix", Title: "fix it", Body: "Closes #42",
	})
	if err != nil {
		t.Fatalf("validatePRRequestClaims: %v", err)
	}
	if title != "fix it" || body != "Closes #42" {
		t.Fatalf("reporter-confirmed bug should remain Closes-able: %q / %q", title, body)
	}
}

// TestValidatePRRequestClaims_HumanFiledBugCloseOnMergeMarkerDoesNotAffectPRClaims
// pins that PR claim validation no longer uses the reporter-confirmation
// markers. Marked and unmarked human-filed bugs keep "Closes #N"; the issue
// close path decides whether the merge may actually close the issue.
func TestValidatePRRequestClaims_HumanFiledBugCloseOnMergeMarkerDoesNotAffectPRClaims(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		labels []map[string]string
	}{
		{
			name:   "marker in body keeps Closes",
			body:   "Found by reading the code ...\n\nhive: close-on-merge",
			labels: []map[string]string{{"name": "kind/bug"}},
		},
		{
			name:   "mixed-case marker in body keeps Closes",
			body:   "Sweep finding\n\nHive: Close-On-Merge",
			labels: []map[string]string{{"name": "kind/bug"}},
		},
		{
			name:   "marker as label keeps Closes",
			body:   "Found by reading the code ...",
			labels: []map[string]string{{"name": "kind/bug"}, {"name": "hive: close-on-merge"}},
		},
		{
			name:   "no marker also keeps Closes",
			body:   "Found by reading the code ...",
			labels: []map[string]string{{"name": "kind/bug"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := map[string]any{
				"number": 42,
				"title":  "sweep finding",
				"body":   tt.body,
				"state":  "open",
				"labels": tt.labels,
				"user":   map[string]any{"login": "some-maintainer", "type": "User"},
			}
			srv := claimValidationServer(t, nil, issue)
			defer srv.Close()
			c := claimValidationClient(t, srv)

			title, body, err := c.validatePRRequestClaims(context.Background(), PRRequest{
				Repo: "o/r", Head: "agent/fix", Title: "fix it", Body: "Closes #42",
			})
			if err != nil {
				t.Fatalf("validatePRRequestClaims: %v", err)
			}
			if title != "fix it" {
				t.Fatalf("title = %q, want unchanged", title)
			}
			if body != "Closes #42" {
				t.Fatalf("human-filed bug should keep Closes regardless of close-gate marker: body = %q", body)
			}
		})
	}
}

// claimTargetServer answers GET /pulls/8 with prState ("" answers 404, the
// lookup error); every other path 404s.
func claimTargetServer(t *testing.T, prState string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/8") && prState != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "state": prState, "user": map[string]any{"login": "Karibhou"}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func liveLedgerClaim(identity string, now time.Time) func(string, int) (issueclaim.Claim, bool) {
	return func(repo string, number int) (issueclaim.Claim, bool) {
		if number != 7 {
			return issueclaim.Claim{}, false
		}
		return issueclaim.Claim{Identity: identity, StartedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), Source: issueclaim.SourceLease}, true
	}
}

var claimTargetRequest = PRRequest{Repo: "o/r", Head: "scanner/fix-7", Title: "fix: thing", Body: "Fixes #7", Agent: "scanner"}

// hivecommons/hive#17298: #17243 was claimed by a contributor and the
// agent's PR closing it was opened anyway.
func TestValidatePRRequestTargets_RefusesContributorClaimedTarget(t *testing.T) {
	srv := claimTargetServer(t, "")
	defer srv.Close()
	c := claimValidationClient(t, srv)
	now := time.Now()
	c.SetIssueClaimLedger(liveLedgerClaim("Karibhou", now))

	refused, err := c.validatePRRequestTargets(context.Background(), claimTargetRequest, now)
	reason, ok := prRequestPolicyReason(err)
	if !ok || !strings.Contains(reason, "already claimed by Karibhou") {
		t.Fatalf("error = %v (%q, policy=%v), want a claim refusal", err, reason, ok)
	}
	if len(refused) != 1 || refused[0].Issue != 7 || refused[0].Holder != "Karibhou" {
		t.Fatalf("refused = %+v, want #7 held by Karibhou", refused)
	}
}

func TestValidatePRRequestTargets_RefusesTargetCoveredByAnotherAuthorsPR(t *testing.T) {
	srv := claimTargetServer(t, "open")
	defer srv.Close()
	c := claimValidationClient(t, srv)
	c.SetPRClaimLookup(ledgerLookupFor(IssueClaim{Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", PRAuthor: "Karibhou", PRState: "open", ExternalAuthor: true, Closing: true}, true))

	refused, err := c.validatePRRequestTargets(context.Background(), claimTargetRequest, time.Now())
	reason, ok := prRequestPolicyReason(err)
	if !ok || !strings.Contains(reason, "covered by #8") {
		t.Fatalf("error = %v (%q, policy=%v), want a coverage refusal", err, reason, ok)
	}
	if len(refused) != 1 || !strings.HasPrefix(refused[0].CoveringPR, "#8") {
		t.Fatalf("refused = %+v, want #7 covered by #8", refused)
	}
}

func TestValidatePRRequestTargets_AllowsOwnClaimAndOwnPR(t *testing.T) {
	srv := claimTargetServer(t, "open")
	defer srv.Close()
	c := claimValidationClient(t, srv)
	now := time.Now()
	c.SetIssueClaimLedger(liveLedgerClaim("scanner", now))
	c.SetPRClaimLookup(ledgerLookupFor(IssueClaim{Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", PRAuthor: "hive[bot]", PRState: "open", Closing: true}, true))

	refused, err := c.validatePRRequestTargets(context.Background(), claimTargetRequest, now)
	if err != nil || len(refused) != 0 {
		t.Fatalf("validatePRRequestTargets = %+v, %v; want the agent's own claim and the hive's own PR allowed", refused, err)
	}
}

func TestValidatePRRequestTargets_RefusesSameNamedContributorAndOtherHiveClaims(t *testing.T) {
	tests := []struct {
		name  string
		claim issueclaim.Claim
	}{
		{"contributor login equal to the agent name", issueclaim.Claim{Identity: "scanner", Kind: "contributor"}},
		{"same-named agent in another hive", issueclaim.Claim{Identity: "scanner", Kind: "agent", Hive: "other-hive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := claimTargetServer(t, "")
			defer srv.Close()
			c := claimValidationClient(t, srv)
			c.SetIssueClaimLedgerHive(func() string { return "this-hive" })
			now := time.Now()
			claim := tt.claim
			claim.StartedAt, claim.ExpiresAt, claim.Source = now.Add(-time.Hour), now.Add(time.Hour), issueclaim.SourceLease
			c.SetIssueClaimLedger(func(repo string, number int) (issueclaim.Claim, bool) { return claim, number == 7 })

			refused, err := c.validatePRRequestTargets(context.Background(), claimTargetRequest, now)
			if _, ok := prRequestPolicyReason(err); !ok || len(refused) != 1 {
				t.Fatalf("validatePRRequestTargets = %+v, %v; want a claim refusal", refused, err)
			}
		})
	}
}

func TestValidatePRRequestTargets_LookupErrorRefuses(t *testing.T) {
	srv := claimTargetServer(t, "")
	defer srv.Close()
	c := claimValidationClient(t, srv)
	c.SetPRClaimLookup(ledgerLookupFor(IssueClaim{Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", PRAuthor: "Karibhou", PRState: "open", ExternalAuthor: true, Closing: true}, true))

	_, err := c.validatePRRequestTargets(context.Background(), claimTargetRequest, time.Now())
	if err == nil {
		t.Fatal("validatePRRequestTargets succeeded although the covering PR could not be read; want fail closed")
	}
	if _, policy := prRequestPolicyReason(err); policy {
		t.Fatalf("error = %v is a policy refusal; want a lookup error the watcher retries", err)
	}
}

// A ledger row whose covering PR has since closed no longer blocks: the
// live re-read, not the stale row, decides.
func TestValidatePRRequestTargets_ClosedCoveringPRNoLongerBlocks(t *testing.T) {
	srv := claimTargetServer(t, "closed")
	defer srv.Close()
	c := claimValidationClient(t, srv)
	c.SetPRClaimLookup(ledgerLookupFor(IssueClaim{Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", PRAuthor: "Karibhou", PRState: "open", ExternalAuthor: true, Closing: true}, true))

	refused, err := c.validatePRRequestTargets(context.Background(), claimTargetRequest, time.Now())
	if err != nil || len(refused) != 0 {
		t.Fatalf("validatePRRequestTargets = %+v, %v; want a closed covering PR to release the target", refused, err)
	}
}

// With issue claims on, the target's claim is read from the forge (marker
// comment, then assignee), the path #17243 actually took.
func TestValidatePRRequestTargets_IssueClaimsEnabledReadsForgeAssignee(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "state": "open",
				"updated_at": now.Add(-time.Minute).UTC().Format(time.RFC3339),
				"assignees":  []map[string]any{{"login": "Karibhou"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := claimValidationClient(t, srv)
	c.SetIssueClaims(func() (bool, time.Duration) { return true, time.Hour })

	refused, err := c.validatePRRequestTargets(context.Background(), claimTargetRequest, now)
	reason, ok := prRequestPolicyReason(err)
	if !ok || !strings.Contains(reason, "already claimed by Karibhou") {
		t.Fatalf("error = %v (%q, policy=%v), want a claim refusal from the forge assignee", err, reason, ok)
	}
	if len(refused) != 1 || refused[0].Issue != 7 || refused[0].Holder != "Karibhou" {
		t.Fatalf("refused = %+v, want #7 held by Karibhou", refused)
	}
}

// With issue claims on, a closing reference to an issue that does not exist
// is permanent: it is rejected (the cross-repo hint validatePRRequestClaims
// gives) rather than failed and retried until the horizon quarantines it.
func TestValidatePRRequestTargets_IssueClaimsEnabledMissingTargetIsPolicyError(t *testing.T) {
	srv := claimTargetServer(t, "")
	defer srv.Close()
	c := claimValidationClient(t, srv)
	c.SetIssueClaims(func() (bool, time.Duration) { return true, time.Hour })

	refused, err := c.validatePRRequestTargets(context.Background(), claimTargetRequest, time.Now())
	reason, ok := prRequestPolicyReason(err)
	if !ok || !strings.Contains(reason, "closing reference o/r#7 does not exist") || !strings.Contains(reason, "owner/repo#N") {
		t.Fatalf("error = %v (%q, policy=%v), want a permanent missing-target rejection", err, reason, ok)
	}
	if len(refused) != 0 {
		t.Fatalf("refused = %+v, want none for a missing target", refused)
	}
}

// With issue claims on, a target that resolves to a pull request carries no
// issue claim: a hive start comment on a contributor-assigned PR is not a
// start on an issue and must not be refused.
func TestIssueRequestStart_IssueClaimsEnabledAllowsCommentOnAssignedPR(t *testing.T) {
	now := time.Now()
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "state": "open",
				"updated_at":   now.Add(-time.Minute).UTC().Format(time.RFC3339),
				"assignees":    []map[string]any{{"login": "Karibhou"}},
				"pull_request": map[string]any{"url": "https://api.github.com/repos/o/r/pulls/7"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var in struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			posted = append(posted, in.Body)
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetIssueClaims(func() (bool, time.Duration) { return true, time.Hour })
	dir := withIssueDir(t)

	p, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 7, Body: "Starting on this now; repro confirmed.", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if _, err := os.Stat(p + ".denied"); err == nil {
		t.Fatal("a start comment on a pull request must not be refused by the claim-target gate")
	}
	if len(posted) != 1 || !strings.Contains(posted[0], "Starting on this") {
		t.Fatalf("comments posted = %q, want the start comment alone", posted)
	}
}

// hivecommons/hive#17298: the kick/start path. A relay comment that announces
// a start on an issue another principal has claimed is refused before the
// comment posts and before notifyAgentStart books the start.
func TestIssueRequestStart_RefusesContributorClaimedTarget(t *testing.T) {
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var in struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			posted = append(posted, in.Body)
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	now := time.Now()
	c.SetIssueClaimLedger(liveLedgerClaim("Karibhou", now))
	calls := recordStarts(c)
	dir := withIssueDir(t)

	p, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 7, Body: "Starting on this now; repro confirmed.", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if _, err := os.Stat(p + ".denied"); err != nil {
		t.Fatalf("start on a contributor-claimed issue must be denied: %v", err)
	}
	b, err := os.ReadFile(strings.TrimSuffix(p, ".json") + ".result.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp IssueResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || !strings.Contains(resp.Error, "o/r#7 is already claimed by Karibhou") {
		t.Fatalf("result = %+v, want a claim refusal naming Karibhou", resp)
	}
	if len(posted) != 1 || !strings.Contains(posted[0], prRequestClaimTargetMarker) || strings.Contains(posted[0], "Starting on this") {
		t.Fatalf("comments posted = %q, want only the claim-target notice, not the start comment", posted)
	}
	// The notice describes the start that was refused, not a PR open that
	// never happened, and still names the holder and the owner's call.
	for _, want := range []string{"asked to start work on this issue", "already claimed by Karibhou", "No work was started", "the owner decides"} {
		if !strings.Contains(posted[0], want) {
			t.Fatalf("notice = %q, want it to contain %q", posted[0], want)
		}
	}
	for _, reject := range []string{"pull request", "No PR was opened"} {
		if strings.Contains(posted[0], reject) {
			t.Fatalf("notice = %q, must not describe a PR open on the start path", posted[0])
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("refused start reported start signals: %+v", *calls)
	}
}

// The claim-target notice names the action that was refused on each path.
func TestNoticePRRequestTargetRefusalBody_NamesAction(t *testing.T) {
	r := prRequestTargetRefusal{Repo: "o/r", Issue: 7, Holder: "Karibhou"}
	cases := []struct {
		action prRequestTargetAction
		asked  string
		result string
	}{
		{prRequestTargetActionOpenPR, "asked to open a pull request that would close this issue", "No PR was opened"},
		{prRequestTargetActionClaim, "asked to claim this issue", "The claim was not recorded"},
		{prRequestTargetActionStart, "asked to start work on this issue", "No work was started"},
	}
	for _, tc := range cases {
		body := noticePRRequestTargetRefusalBody(tc.action, r)
		for _, want := range []string{prRequestClaimTargetMarker, tc.asked, "already claimed by Karibhou", tc.result, "the owner decides"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: notice = %q, want it to contain %q", tc.action, body, want)
			}
		}
	}
	if got := issueRequestStartTargetAction("claim"); got != prRequestTargetActionClaim {
		t.Errorf("issueRequestStartTargetAction(claim) = %q", got)
	}
	if got := issueRequestStartTargetAction("comment"); got != prRequestTargetActionStart {
		t.Errorf("issueRequestStartTargetAction(comment) = %q", got)
	}
}

// The agent's own live claim lets its start comment through and books the
// start, as before.
func TestIssueRequestStart_AllowsOwnClaimedTarget(t *testing.T) {
	commented := 0
	srv := newIssueMockServer(t, "", nil, nil, &commented)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	now := time.Now()
	c.SetIssueClaimLedger(liveLedgerClaim("scanner", now))
	calls := recordStarts(c)
	dir := withIssueDir(t)

	if _, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 7, Body: "Starting on this now; repro confirmed.", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if commented != 1 {
		t.Fatalf("expected the start comment to post, got %d", commented)
	}
	want := startCall{agent: "scanner", repo: "o/r", number: 7, signal: AgentStartSignalComment}
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("start signals = %+v, want %+v", *calls, want)
	}
}
