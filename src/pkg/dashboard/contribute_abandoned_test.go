package dashboard

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github/automerge"
)

func abandonFixture(t *testing.T) {
	t.Helper()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "Mallory", ContributorID: "c-mal", TrustTier: "contributor"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestRecordContributorAbandonDedupesPerPR(t *testing.T) {
	abandonFixture(t)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	// In place first: counted once, no copy yet.
	counted, err := recordContributorAbandon(automerge.ContributorAbandon{Author: "mallory", Repo: "acme/widget", PR: 7, Reason: automerge.AbandonReasonOffline, At: at})
	if err != nil || !counted {
		t.Fatalf("first adoption: counted=%v err=%v", counted, err)
	}
	// Re-homed onto hive/adopt-7: fills in the copy, does not count again.
	counted, err = recordContributorAbandon(automerge.ContributorAbandon{Author: "mallory", Repo: "Acme/Widget", PR: 7, AdoptedIntoPR: 90, Reason: automerge.AbandonReasonUnresponsive, At: at.Add(time.Hour)})
	if err != nil || counted {
		t.Fatalf("re-home: counted=%v err=%v", counted, err)
	}
	// The same PR adopted again: no-op.
	if counted, err = recordContributorAbandon(automerge.ContributorAbandon{Author: "mallory", Repo: "acme/widget", PR: 7, AdoptedIntoPR: 91, Reason: automerge.AbandonReasonOperatorTakeover, At: at.Add(2 * time.Hour)}); err != nil || counted {
		t.Fatalf("second adoption: counted=%v err=%v", counted, err)
	}
	// A different PR: counted.
	if counted, err = recordContributorAbandon(automerge.ContributorAbandon{Author: "@mallory", Repo: "acme/widget", PR: 8, AdoptedIntoPR: 92, Reason: automerge.AbandonReasonOperatorTakeover, At: at}); err != nil || !counted {
		t.Fatalf("second PR: counted=%v err=%v", counted, err)
	}

	p, err := loadContributorProfile("Mallory")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.Abandoned != 2 || len(p.AbandonedPRs) != 2 {
		t.Fatalf("abandoned=%d records=%+v", p.Abandoned, p.AbandonedPRs)
	}
	want := ContributorAbandonRecord{Repo: "acme/widget", PR: 7, AdoptedIntoPR: 90, Reason: automerge.AbandonReasonOffline, At: "2026-10-30T12:00:00Z"}
	if p.AbandonedPRs[0] != want {
		t.Fatalf("first record = %+v, want %+v", p.AbandonedPRs[0], want)
	}
	if r := p.AbandonedPRs[1]; r.PR != 8 || r.AdoptedIntoPR != 92 || r.Reason != automerge.AbandonReasonOperatorTakeover {
		t.Fatalf("second record = %+v", r)
	}
}

func TestRecordContributorAbandonIgnoresUnknownAuthor(t *testing.T) {
	abandonFixture(t)
	counted, err := recordContributorAbandon(automerge.ContributorAbandon{Author: "stranger", Repo: "acme/widget", PR: 7, Reason: automerge.AbandonReasonUnresponsive})
	if err != nil || counted {
		t.Fatalf("unknown author: counted=%v err=%v", counted, err)
	}
	if _, err := loadContributorProfile("stranger"); err == nil {
		t.Fatal("a profile was created for an unregistered author")
	}
}

// A writer holding a copy loaded before the adoption must not drop it.
func TestContributorAbandonSurvivesStaleSave(t *testing.T) {
	abandonFixture(t)
	stale, err := loadContributorProfile("Mallory")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	stale.Version = 0
	(&ContributeWSHub{}).Abandoned(context.Background(), automerge.ContributorAbandon{Author: "mallory", Repo: "acme/widget", PR: 7, AdoptedIntoPR: 90, Reason: automerge.AbandonReasonOffline})
	stale.TasksCompleted++
	if err := saveContributorProfile(stale); err != nil {
		t.Fatalf("stale save: %v", err)
	}
	p, _ := loadContributorProfile("Mallory")
	if p.Abandoned != 1 || len(p.AbandonedPRs) != 1 || p.TasksCompleted != 1 {
		t.Fatalf("after stale save: abandoned=%d records=%+v tasks=%d", p.Abandoned, p.AbandonedPRs, p.TasksCompleted)
	}
}

func TestContributorsListExposesAbandoned(t *testing.T) {
	srv := newFullServer(t)
	abandonFixture(t)
	if _, err := recordContributorAbandon(automerge.ContributorAbandon{Author: "Mallory", Repo: "acme/widget", PR: 7, AdoptedIntoPR: 90, Reason: automerge.AbandonReasonOperatorTakeover}); err != nil {
		t.Fatalf("record: %v", err)
	}
	w := httptest.NewRecorder()
	srv.handleContributorsList(w, httptest.NewRequest("GET", "/api/contributors", nil))
	var body struct {
		Contributors []struct {
			GitHubUsername string                     `json:"github_username"`
			Abandoned      *int                       `json:"abandoned"`
			AbandonedPRs   []ContributorAbandonRecord `json:"abandoned_prs"`
		} `json:"contributors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	if len(body.Contributors) != 1 {
		t.Fatalf("contributors = %s", w.Body.String())
	}
	c := body.Contributors[0]
	if c.Abandoned == nil || *c.Abandoned != 1 || len(c.AbandonedPRs) != 1 || c.AbandonedPRs[0].AdoptedIntoPR != 90 || c.AbandonedPRs[0].Reason != "operator_takeover" {
		t.Fatalf("contributor = %s", w.Body.String())
	}
}
