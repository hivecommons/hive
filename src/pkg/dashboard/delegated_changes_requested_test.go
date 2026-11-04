package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// projectbluefin/testsuite#1057: the delegate approved a green,
// conflict-free fork PR "as it is" while a maintainer's CHANGES_REQUESTED
// review stood on its head. The gather reads the standing requests the way
// GitHub's reviewDecision counts them, and the decision names them.
func TestGatherDelegatedStandingChangeRequests(t *testing.T) {
	old := time.Now().Add(-72 * time.Hour)
	review := func(login, state, assoc string, at time.Time) map[string]any {
		return map[string]any{"user": map[string]string{"login": login}, "state": state, "author_association": assoc, "commit_id": "887f0dd", "submitted_at": at}
	}
	reviews := []map[string]any{
		review("hive[bot]", "CHANGES_REQUESTED", "MEMBER", old),           // the swarm's own: read from the artifact instead
		review("drive-by", "CHANGES_REQUESTED", "NONE", old),              // no maintainer: GitHub does not block on it
		review("cleared-by-approval", "CHANGES_REQUESTED", "MEMBER", old), // later approved
		review("cleared-by-approval", "APPROVED", "MEMBER", old.Add(time.Minute)),
		review("dismissed", "CHANGES_REQUESTED", "COLLABORATOR", old), // later dismissed
		review("dismissed", "DISMISSED", "COLLABORATOR", old.Add(time.Minute)),
		review("Danathar", "CHANGES_REQUESTED", "MEMBER", old.Add(2*time.Minute)),
		review("Danathar", "COMMENTED", "MEMBER", old.Add(3*time.Minute)), // a comment does not clear it
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/1057"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1057, "state": "open", "pull_request": map[string]any{}, "user": map[string]string{"login": "mendezr"}})
		case strings.HasSuffix(r.URL.Path, "/pulls/1057"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1057, "state": "open", "user": map[string]string{"login": "mendezr"}, "head": map[string]string{"sha": "887f0dd"}, "created_at": old, "mergeable": true, "mergeable_state": "clean"})
		case strings.HasSuffix(r.URL.Path, "/pulls/1057/reviews"):
			_ = json.NewEncoder(w).Encode(reviews)
		case strings.Contains(r.URL.Path, "/permission"):
			_, _ = w.Write([]byte(`{"permission":"read"}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	s := newFullServer(t)
	s.deps.GHClient = github.NewClient("token", "projectbluefin", nil, slog.Default(), server.URL)
	s.deps.GHClient.SetAppBotLogin("hive[bot]")
	s.status = &StatusPayload{}
	in, err := s.gatherDelegatedInput(context.Background(), "projectbluefin/testsuite", 1057, "pr", delegatedDecisionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Danathar"}; !reflect.DeepEqual(in.ChangesRequestedBy, want) {
		t.Fatalf("ChangesRequestedBy = %v, want %v", in.ChangesRequestedBy, want)
	}
	if !in.CIGreen || in.Conflicting {
		t.Fatalf("the head should read green and conflict-free: %+v", in)
	}
	d := delegatedecision.Run(in)
	if d.Verdict == delegatedecision.VerdictApprove || d.Rule == delegatedecision.RuleMergeableAsIs {
		t.Fatalf("approved over @Danathar's change request: %+v", d)
	}
	if !strings.Contains(d.Text, "@Danathar") {
		t.Fatalf("decision does not name the request: %q", d.Text)
	}
}

// A review list GitHub will not serve stops the decision rather than
// reading as "nothing requested". The PR is the hive's own, so the
// contributor-activity read (which lists reviews too) is not what fails.
func TestGatherDelegatedReviewsUnreadable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/5"):
			_, _ = w.Write([]byte(`{"number":5,"state":"open","pull_request":{},"user":{"login":"hive[bot]"}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/5"):
			_, _ = w.Write([]byte(`{"number":5,"state":"open","user":{"login":"hive[bot]"},"head":{"sha":"abc"},"mergeable":true,"mergeable_state":"clean","created_at":"2020-01-01T00:00:00Z"}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/5/reviews"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		case strings.Contains(r.URL.Path, "/permission"):
			_, _ = w.Write([]byte(`{"permission":"read"}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	s := newFullServer(t)
	s.deps.GHClient = github.NewClient("token", "org", nil, slog.Default(), server.URL)
	s.deps.GHClient.SetAppBotLogin("hive[bot]")
	s.status = &StatusPayload{}
	if _, err := s.gatherDelegatedInput(context.Background(), "org/repo", 5, "pr", delegatedDecisionRequest{}); err == nil || !strings.Contains(err.Error(), "reading reviews") {
		t.Fatalf("err = %v, want a reviews read error", err)
	}
}

// The swarm's verdict counts only on the head it reviewed: a change
// request on an older head says nothing about the one being decided.
func TestGatherDelegatedHeadReviewVerdict(t *testing.T) {
	orig := review.ReviewVerdictsPath
	review.ReviewVerdictsPath = t.TempDir() + "/review-verdicts.json"
	t.Cleanup(func() { review.ReviewVerdictsPath = orig })
	art := review.Artifact{Items: []review.Aggregate{{
		// DecisionRequested: the parked-row fallback would offer it for
		// any head; HeadReviewVerdict must not take it from there.
		Repo: "projectbluefin/testsuite", Number: 1057, HeadSHA: "887f0dd", Verdict: review.VerdictChangesRequested, DecisionRequested: true, RecordedAt: time.Now().UTC(),
	}}}
	if err := review.WriteArtifact("", art); err != nil {
		t.Fatal(err)
	}
	s := newFullServer(t)
	for head, want := range map[string]string{"887f0dd": "changes_requested", "72ab005": ""} {
		in := delegatedecision.Input{Repo: "projectbluefin/testsuite", Number: 1057, Kind: delegatedecision.KindPR}
		s.gatherDelegatedRecordedPark("projectbluefin/testsuite", 1057, head, &in)
		if in.HeadReviewVerdict != want {
			t.Errorf("head %s: HeadReviewVerdict = %q, want %q", head, in.HeadReviewVerdict, want)
		}
	}
}
