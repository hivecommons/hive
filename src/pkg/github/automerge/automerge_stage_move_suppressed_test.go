package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// gatedTransport is stubTransport with the process write-back gate.
type gatedTransport struct {
	stubTransport
	gate *writeback.Gate
}

func (t gatedTransport) WritebackGate() *writeback.Gate { return t.gate }

// projectbluefin/fsdk-containers#491 (a fork PR nobody opted in): the hive
// posted "Moving to `stage/085-needs-decision` so a person sees it" twice
// while the write-back gate kept every stage label off the PR. Each
// escalation note now says the hive would move it, why it did not, and
// where the stage is tracked; an opted-in PR keeps the plain sentence.
func TestEscalationNoteSaysWhenTheStageLabelIsRefused(t *testing.T) {
	gate := writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 5 }}, "")
	e := &Engine{transport: gatedTransport{gate: gate}}
	obs := stageObservation{Display: "projectbluefin/fsdk-containers", Owner: "projectbluefin", Repo: "fsdk-containers", Number: 491, HeadSHA: "c0ad5df0927de888c373ccf9b5caead751ef9a6f", Author: "kylerankin", HeadRepo: "kylerankin/fsdk-containers", OpenedAt: time.Now().Add(time.Hour)}
	label := stagelabels.NeedsDecision.Label()
	notes := map[string]string{
		"adoption stalled": AdoptionStalledComment(obs, stagelabels.Record{}, "adoption-stalled:reviewer-verdict:changes_requested", 12*time.Hour),
		"workflow refused": AdoptionWorkflowRefusedComment(obs),
		"fork not adopted": ForkNotAdoptedComment(obs, "adoption-stalled:fork-rehome-refused", 30*time.Hour),
	}
	for name, body := range notes {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(body, "Moving to `"+label+"`") {
				t.Fatalf("fixture no longer announces the move:\n%s", body)
			}
			got := e.withStageMoveFact(obs, body)
			if strings.Contains(got, "Moving to `") {
				t.Fatalf("a refused label write is still announced as a move:\n%s", got)
			}
			for _, want := range []string{"would move this to `" + label + "`", "write-back gate", string(writeback.ReasonNeedsLabel), "tracks it at `" + label + "`"} {
				if !strings.Contains(got, want) {
					t.Fatalf("note lacks %q:\n%s", want, got)
				}
			}
			opted := obs
			opted.Labels = []string{writeback.DefaultOptInLabel}
			if got := e.withStageMoveFact(opted, body); got != body {
				t.Fatalf("an opted-in PR's note must be unchanged:\n%s", got)
			}
		})
	}
	if n := len(gate.Recent()); n != 0 {
		t.Fatalf("describing the move recorded %d suppression(s); the stage writer records its own", n)
	}
	// No gate installed: every write is allowed, the note is unchanged.
	if got := (&Engine{transport: stubTransport{}}).withStageMoveFact(obs, notes["workflow refused"]); got != notes["workflow refused"] {
		t.Fatalf("no gate: note changed:\n%s", got)
	}
}

// The note the hive actually posts goes through the same check.
func TestPostedEscalationNoteSaysWhenTheStageLabelIsRefused(t *testing.T) {
	var posted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = body.Body
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	client := gh.NewClient(nil)
	client.BaseURL, _ = url.Parse(srv.URL + "/")
	gate := writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 5 }}, "")
	e := &Engine{transport: gatedTransport{gate: gate}, gh: client}
	store := stagelabels.NewStore("", time.Now)
	obs := stageObservation{Display: "projectbluefin/fsdk-containers", Owner: "projectbluefin", Repo: "fsdk-containers", Number: 491, HeadSHA: "c0ad5df", Author: "kylerankin", OpenedAt: time.Now().Add(time.Hour)}
	if n, err := e.postAdoptionEscalationNote(context.Background(), store, stagelabels.Key("projectbluefin/fsdk-containers", 491), obs, adoptionWorkflowRefusedReason, AdoptionWorkflowRefusedComment(obs), "failed"); n != 1 || err != nil {
		t.Fatalf("post = (%d, %v)", n, err)
	}
	if strings.Contains(posted, "Moving to `") || !strings.Contains(posted, "would move this to `"+stagelabels.NeedsDecision.Label()+"`") {
		t.Fatalf("posted note announces a refused move:\n%s", posted)
	}
}
