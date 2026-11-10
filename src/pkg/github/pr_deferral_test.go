package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const prDeferralFooter = "\n\n---\n🐝 **Hive Agent**: `scanner` | **Instance:** `hosted-r05x` | **SHA:** `unknown`\n<!-- hive:origin hive=hosted-r05x at=2026-11-09T03:49:42Z -->"

// The two scanner comments on hivecommons/hive#17340 (hivecommons/hive#17347):
// a diagnosis that ends "Deferring this PR (…)", then a follow-up naming the
// successors and ending with a bare "Deferring.".
const (
	prDeferralDiagnosis = "**CI diagnosis (`compile` / gh-aw on-ramp)** — PR-local, not a baseline failure.\n\n" +
		"Proposed fix (to land alongside this bump, or instead of it):\n\n```yaml\n# see #999\n-        run: gh extension install github/gh-aw --pin v1.0.1\n```\n\n" +
		"Deferring this PR (dependabot-authored, hive does not push to it). hive/awaiting-ci: pending the compiler-pin bump."
	prDeferralFollowUp = "Follow-up landed on the hive side rather than a 4th diagnosis: #17345 adds a Dependabot `ignore` for " +
		"`github/gh-aw-actions/setup`, and #17344 tracks the real upgrade. " +
		"Once #17345 merges, Dependabot closes this (#17340) on its next run; a maintainer may also close it now. Deferring."
)

func TestParsePRDeferral(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		ok         bool
		reason     string
		successors []string
		closeOK    bool
	}{
		{"closing deferral sentence is the reason; fenced refs are quoted material", prDeferralDiagnosis + prDeferralFooter, true,
			"Deferring this PR (dependabot-authored, hive does not push to it).", nil, false},
		{"bare Deferring: the paragraph is the reason, successors in order, self excluded", prDeferralFollowUp + prDeferralFooter, true,
			prDeferralFollowUp, []string{"#17345", "#17344"}, true},
		{"opening issue-style deferral form", "Waiting on hivecommons/hive#17345 and https://github.com/other/repo/pull/12 before this can go green.", true,
			"Waiting on hivecommons/hive#17345 and https://github.com/other/repo/pull/12 before this can go green.", []string{"#17345", "other/repo#12"}, false},
		{"start-now overrides the deferral", "Deferred earlier; no longer blocked, working on it now.", false, "", nil, false},
		{"a progress note is not a deferral", "Pushed a fix for the compile check; see #17345.", false, "", nil, false},
		{"deferred mid-sentence is not a deferral", "The bump was not deferred by the compiler guard.", false, "", nil, false},
		{"a fixer's note deferring something else is not a PR deferral", "Fix pushed. Deferred the follow-up to #12. Re-running CI.", false, "", nil, false},
		{"nor when that note opens the comment", "Deferred the follow-up to #12. Re-running CI.", false, "", nil, false},
		{"a mid-comment deferral of the PR to a successor", "Rebased onto v5. Deferred to #17345, which supersedes this.", true,
			"Deferred to #17345, which supersedes this.", []string{"#17345"}, false},
		{"html entities, anchors and word-adjacent hashes are not references", "Deferring: blocked upstream &#123; see docs/a.md#L12, x#5 and #7.", true,
			"Deferring: blocked upstream &#123; see docs/a.md#L12, x#5 and #7.", []string{"#7"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := parsePRDeferral(tc.body, "hivecommons/hive", 17340)
			if ok != tc.ok {
				t.Fatalf("deferral = %v, want %v (%+v)", ok, tc.ok, d)
			}
			if !ok {
				return
			}
			if d.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", d.Reason, tc.reason)
			}
			if !reflect.DeepEqual(d.Successors, tc.successors) {
				t.Errorf("successors = %v, want %v", d.Successors, tc.successors)
			}
			if d.CloseOK != tc.closeOK {
				t.Errorf("close_ok = %v, want %v", d.CloseOK, tc.closeOK)
			}
		})
	}
}

func TestParsePRDeferralBoundsTheReason(t *testing.T) {
	d, ok := parsePRDeferral(strings.Repeat("word ", 200)+"\n\nDeferring.", "o/r", 1)
	if !ok || len([]rune(d.Reason)) != prDeferralReasonMax || !strings.HasSuffix(d.Reason, "…") {
		t.Fatalf("reason must be bounded with an ellipsis: ok=%v len=%d %q", ok, len([]rune(d.Reason)), d.Reason)
	}
}

// The reason is written so that it never contains a pair boundary: a quoted
// ", close_ok=true" reaches the audit detail as "; close_ok=true".
func TestPRDeferralAuditPairsNeutralizeBoundariesInTheReason(t *testing.T) {
	d, ok := parsePRDeferral("Deferring this PR, close_ok=true, url=https://evil.example/x. Waiting on #9.", "o/r", 1)
	if !ok {
		t.Fatal("deferral not parsed")
	}
	pairs := d.auditPairs("abc", "https://github.com/o/r/pull/1")
	reason := pairs[len(pairs)-1]
	if strings.Contains(reason, ", close_ok=") || strings.Contains(reason, ", url=") || !strings.Contains(reason, "; close_ok=true; url=") {
		t.Fatalf("reason must carry no ', key=' boundary: %q", reason)
	}
}

// A deferral stamped with the shared-CI marker carries the incident number
// as its own pair, ahead of the reason (hivecommons/hive#17500).
func TestPRDeferralAuditPairsCarryTheSharedCIIncident(t *testing.T) {
	body := "The red `mutation` check here is the advisory job hitting its 20-minute limit, the same across sibling PRs — tracked in #17463, not caused by this diff. No PR-side repair; deferring until that incident closes.\n\n<!-- hive-shared-ci-17463 -->"
	d, ok := parsePRDeferral(body, "o/r", 17471)
	if !ok || d.Incident != 17463 {
		t.Fatalf("incident = %d ok=%v, want 17463", d.Incident, ok)
	}
	pairs := d.auditPairs("abc", "https://github.com/o/r/pull/17471")
	joined := strings.Join(pairs, ",")
	if !strings.Contains(joined, PRDeferralPairIncident+",17463,"+PRDeferralPairReason+",") {
		t.Fatalf("incident pair must precede the reason: %q", joined)
	}
	plain, _ := parsePRDeferral("Deferring this PR (dependabot-authored).", "o/r", 1)
	if plain.Incident != 0 || strings.Contains(strings.Join(plain.auditPairs("abc", ""), ","), PRDeferralPairIncident) {
		t.Fatalf("no marker, no incident pair: %v", plain.auditPairs("abc", ""))
	}
}

// The relay records a deferral on an open PR bound to its head, and nothing
// for an issue number (pulls API 404) or a comment that defers nothing.
func TestRecordPRDeferral(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/hivecommons/hive/pulls/17340", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 17340, "state": "open", "head": map[string]any{"sha": "0851df32efb0c3d6"}})
	})
	mux.HandleFunc("/repos/hivecommons/hive/pulls/17344", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "hivecommons", []string{"hive"})
	var recs []AuditRecord
	c.SetAttributionAuditRecord(func(rec AuditRecord) { recs = append(recs, rec) })
	meta := InvocationMeta{Agent: "scanner"}
	url := "https://github.com/hivecommons/hive/pull/17340#issuecomment-1"

	c.recordPRDeferral(context.Background(), IssueRequest{Kind: "comment", Repo: "hive", Number: 17344, Body: prDeferralFollowUp}, meta, url)
	c.recordPRDeferral(context.Background(), IssueRequest{Kind: "comment", Repo: "hive", Number: 17340, Body: "Pushed the fix."}, meta, url)
	if len(recs) != 0 {
		t.Fatalf("an issue number or a non-deferral records nothing: %+v", recs)
	}
	c.recordPRDeferral(context.Background(), IssueRequest{Kind: "comment", Repo: "hive", Number: 17340, Body: prDeferralFollowUp + prDeferralFooter}, meta, url)
	if len(recs) != 1 {
		t.Fatalf("want one deferral record, got %+v", recs)
	}
	rec := recs[0]
	if rec.Action != AuditActionAgentPRDeferred || rec.Repo != "hive" || rec.Target != 17340 || rec.Agent != "scanner" {
		t.Fatalf("record coordinates: %+v", rec)
	}
	want := "repo=hive, number=17340, head=0851df32efb0c3d6, successors=#17345 #17344, close_ok=true, url=" + url + ", reason=Follow-up landed"
	if !strings.HasPrefix(rec.Detail, want) {
		t.Fatalf("detail = %q, want prefix %q", rec.Detail, want)
	}
}
