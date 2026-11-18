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
			got := e.withStageMoveFact(context.Background(), obs, body)
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
			if got := e.withStageMoveFact(context.Background(), opted, body); got != body {
				t.Fatalf("an opted-in PR's note must be unchanged:\n%s", got)
			}
		})
	}
	if n := len(gate.Recent()); n != 0 {
		t.Fatalf("describing the move recorded %d suppression(s); the stage writer records its own", n)
	}
	// No gate installed: every write is allowed, the note is unchanged.
	if got := (&Engine{transport: stubTransport{}}).withStageMoveFact(context.Background(), obs, notes["workflow refused"]); got != notes["workflow refused"] {
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

// hiveStageTransport is gatedTransport whose client says whether a PR's
// stage labels are the hive's own (hgithub.Client.HiveAppliedStageLabels).
type hiveStageTransport struct {
	gatedTransport
	applied bool
	asked   *int
}

func (t hiveStageTransport) HiveAppliedStageLabels(context.Context, string, int, []string) bool {
	*t.asked++
	return t.applied
}

// projectbluefin/utah#1274: on a PR older than the backfill cutoff the note
// said the gate kept the hive's labels off, while the 074 on it was the
// hive's own. The stage writer now moves a stage label the hive applied, so
// the note asks with the same fact and announces the move; a person's stage
// label is still described as held by the backfill guard. A PR the guard
// does not cover is decided without asking.
func TestEscalationNoteAnnouncesTheMoveOfAHiveAppliedStageLabel(t *testing.T) {
	firstSeen := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cfg := writeback.Config{Kinds: map[string]writeback.KindPolicyOverride{"stage_label": {RequireLabel: gh.Ptr(false)}}}
	gate := writeback.New(cfg, writeback.Deps{LevelFor: func(string) int { return 6 }, Now: func() time.Time { return firstSeen }}, "")
	gate.ObserveRepo("projectbluefin/utah")
	obs := stageObservation{Display: "projectbluefin/utah", Owner: "projectbluefin", Repo: "utah", Number: 1274, HeadSHA: "abc1274", Author: "contributor", Labels: []string{"stage/074-adopted-by-hive"}, OpenedAt: firstSeen.Add(-time.Hour)}
	body := AdoptionWorkflowRefusedComment(obs)
	label := stagelabels.NeedsDecision.Label()

	asked := 0
	hive := &Engine{transport: hiveStageTransport{gatedTransport: gatedTransport{gate: gate}, applied: true, asked: &asked}}
	if got := hive.withStageMoveFact(context.Background(), obs, body); got != body {
		t.Fatalf("the hive's own stage label: note must announce the move unchanged:\n%s", got)
	}
	person := &Engine{transport: hiveStageTransport{gatedTransport: gatedTransport{gate: gate}, applied: false, asked: &asked}}
	got := person.withStageMoveFact(context.Background(), obs, body)
	if !strings.Contains(got, "would move this to `"+label+"`") || !strings.Contains(got, string(writeback.ReasonBackfill)) {
		t.Fatalf("a person's stage label: note must say the backfill guard holds the move:\n%s", got)
	}
	if asked != 2 {
		t.Fatalf("asked %d times, want once per old PR note", asked)
	}
	newer := obs
	newer.OpenedAt = firstSeen.Add(time.Hour)
	if got := person.withStageMoveFact(context.Background(), newer, body); got != body || asked != 2 {
		t.Fatalf("a PR the guard does not cover: note changed or the fact was asked (asked=%d):\n%s", asked, got)
	}
	if n := len(gate.Recent()); n != 0 {
		t.Fatalf("describing the move recorded %d suppression(s)", n)
	}
}
