package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// governor.labels.stage.adoption.person_after / person_grace / person_repos
// / person_human_repos (hivecommons/hive#19395) are edited on the Labels tab
// and saved through PUT /api/config/governor/labels.
func TestHandleGovernorLabels_PersonTakeoverRoundTrip(t *testing.T) {
	srv := newFullServer(t)
	adoption := func() config.StageAdoptionConfig { return srv.deps.Config.Governor.Labels.Stage.Adoption }
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", body))
		return w
	}

	if w := put(`{"stage_adoption_person_after":" 24h ","stage_adoption_person_grace":"6h","stage_adoption_person_repos":["testrepo"],"stage_adoption_person_human_repos":["testorg/testrepo"]}`); w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	a := adoption()
	if a.PersonAfter != "24h" || a.PersonGrace != "6h" || !reflect.DeepEqual(a.PersonRepos, []string{"testorg/testrepo"}) || !reflect.DeepEqual(a.PersonHumanRepos, []string{"testorg/testrepo"}) {
		t.Fatalf("saved %+v", a)
	}
	got := decodeGovernorConfigGet(t, srv)
	if got["stageAdoptionPersonAfter"] != "24h" || got["stageAdoptionPersonGrace"] != "6h" {
		t.Fatalf("GET after/grace = %v / %v", got["stageAdoptionPersonAfter"], got["stageAdoptionPersonGrace"])
	}
	for _, key := range []string{"stageAdoptionPersonRepos", "stageAdoptionPersonHumanRepos"} {
		if list, ok := got[key].([]any); !ok || len(list) != 1 || list[0] != "testorg/testrepo" {
			t.Fatalf("GET %s = %#v", key, got[key])
		}
	}

	// A save that touches none of the keys leaves them alone.
	if w := put(`{"stage_nudge_ci_failure_cooldown":"2h"}`); w.Code != http.StatusOK {
		t.Fatalf("unrelated save: code = %d", w.Code)
	}
	if b := adoption(); !reflect.DeepEqual(a, b) {
		t.Fatalf("unrelated save changed %+v to %+v", a, b)
	}

	for _, bad := range []string{
		`{"stage_adoption_person_after":"soon"}`,
		`{"stage_adoption_person_after":"-1h"}`,
		`{"stage_adoption_person_grace":"later"}`,
		`{"stage_adoption_person_grace":"-1h"}`,
		`{"stage_adoption_person_repos":["elsewhere/unwatched"]}`,
		`{"stage_adoption_person_human_repos":["elsewhere/unwatched"]}`,
	} {
		if w := put(bad); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, want 400; body = %s", bad, w.Code, w.Body.String())
		}
	}
	if b := adoption(); !reflect.DeepEqual(a, b) {
		t.Fatalf("rejected saves mutated %+v to %+v", a, b)
	}

	// "0" turns the timer off and puts the grace back to its 24h default;
	// empty lists clear the opt-ins.
	if w := put(`{"stage_adoption_person_after":"0","stage_adoption_person_grace":"0","stage_adoption_person_repos":[],"stage_adoption_person_human_repos":[]}`); w.Code != http.StatusOK {
		t.Fatalf("clear: code = %d, body = %s", w.Code, w.Body.String())
	}
	a = adoption()
	if a.PersonAfter != "" || a.PersonGrace != "" || len(a.PersonRepos) != 0 || len(a.PersonHumanRepos) != 0 {
		t.Fatalf("clear left %+v", a)
	}
	if a.EffectivePersonAfter() != 0 || a.EffectivePersonGrace() != config.DefaultPersonTakeoverGrace {
		t.Fatalf("cleared timer reads after=%v grace=%v", a.EffectivePersonAfter(), a.EffectivePersonGrace())
	}
}

// The Labels tab carries all four controls next to abandon_after, each
// with its YAML key and help mark, and the settings search finds them.
func TestLabelsTabHasPersonTakeoverControls(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("static-src", "js", "config-governor.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	for _, want := range []string{
		`data-arg1="stage_adoption_person_after"`,
		`yamlHint('governor.labels.stage.adoption.person_after')`,
		`settingHelpMark('settings', 'labels', 'Person PR takeover timer'`,
		`data-arg1="stage_adoption_person_grace"`,
		`yamlHint('governor.labels.stage.adoption.person_grace')`,
		`settingHelpMark('settings', 'labels', 'Person PR takeover grace'`,
		`markDirtyStagePersonRepos`,
		`yamlHint('governor.labels.stage.adoption.person_repos[]')`,
		`settingHelpMark('settings', 'labels', 'Take over owners agent PRs'`,
		`markDirtyStagePersonHumanRepos`,
		`yamlHint('governor.labels.stage.adoption.person_human_repos[]')`,
		`settingHelpMark('settings', 'labels', 'Take over hand-written PRs'`,
		`stageAdoptionPersonAfter`,
		`stageAdoptionPersonGrace`,
		`stageAdoptionPersonRepos`,
		`stageAdoptionPersonHumanRepos`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("config-governor.js lacks %s", want)
		}
	}
	if i, j := strings.Index(js, `id="cfg-stage-abandon-close-repos"`), strings.Index(js, `id="cfg-stage-person-after"`); i < 0 || j < i {
		t.Error("the takeover timer controls must sit after the abandon_after controls")
	}
	html := indexHTML(t)
	for _, fn := range []string{"function markDirtyStagePersonRepos(", "function markDirtyStagePersonHumanRepos(", "function stageRepoSelectOptions("} {
		if !strings.Contains(html, fn) {
			t.Errorf("index.html lacks %s; regenerate it from static-src", fn)
		}
	}
	if !strings.Contains(html, "key: 'governor.labels.stage.adoption.person_after'") {
		t.Error("the settings search has no entry for governor.labels.stage.adoption.person_after")
	}
}

// personTakeoverServer is a server whose config opts acme/widget into the
// takeover timer for both kinds at L6, with owner as a hive owner.
func personTakeoverServer(after, grace string) *Server {
	level := 6
	cfg := &config.Config{ACMMLevel: &level}
	cfg.Dashboard.AuthorizedUsers = []string{"owner:owner"}
	cfg.Governor.Labels.Stage.Adoption.PersonAfter = after
	cfg.Governor.Labels.Stage.Adoption.PersonGrace = grace
	cfg.Governor.Labels.Stage.Adoption.PersonRepos = []string{"acme/widget"}
	cfg.Governor.Labels.Stage.Adoption.PersonHumanRepos = []string{"acme/widget"}
	s := newTestServer()
	s.deps = &Dependencies{Config: cfg}
	return s
}

// The person_takeover field is set only for a person's 071 / 072 PR in an
// opted-in repository, and says when the hive takes it over.
func TestStageTrackerPersonTakeoverField(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	human := StageTrackerItem{Kind: "pr", Repo: "acme/widget", Number: 7, Stage: stagelabels.NeedsAuthor.Number(), Author: "someone", AuthorKind: authorKindHuman}
	ownerAgent := StageTrackerItem{Kind: "pr", Repo: "acme/widget", Number: 8, Stage: stagelabels.ReturnedToContributor.Number(), Author: "owner", AuthorKind: authorKindClanker}
	s := personTakeoverServer("24h", "")

	pt := s.stageTrackerPersonTakeover(human, stagelabels.Record{}, false, now)
	if pt == nil || pt.After != "24h" || pt.Grace != "24h" || pt.NoticeAt != nil || pt.At != nil {
		t.Fatalf("before the notice: %+v", pt)
	}
	if !strings.Contains(pt.Text, "takeover timer is on") || !strings.Contains(pt.Text, "after 24h") || !strings.Contains(pt.Text, "24h later") {
		t.Fatalf("before-notice text = %q", pt.Text)
	}
	if s.stageTrackerPersonTakeover(ownerAgent, stagelabels.Record{}, false, now) == nil {
		t.Fatal("an owner's agent PR in person_repos has no person_takeover")
	}

	noticed := now.Add(-2 * time.Hour)
	pt = s.stageTrackerPersonTakeover(human, stagelabels.Record{PersonNoticeAt: noticed}, true, now)
	if pt == nil || pt.NoticeAt == nil || !pt.NoticeAt.Equal(noticed) || pt.At == nil || !pt.At.Equal(noticed.Add(24*time.Hour)) {
		t.Fatalf("after the notice: %+v", pt)
	}
	if !strings.Contains(pt.Text, "posted its takeover notice") || !strings.Contains(pt.Text, "in 22h") {
		t.Fatalf("after-notice text = %q", pt.Text)
	}

	for name, tc := range map[string]struct {
		s  *Server
		it StageTrackerItem
	}{
		"timer off":           {personTakeoverServer("", ""), human},
		"not a person":        {s, StageTrackerItem{Kind: "pr", Repo: "acme/widget", Number: 9, Stage: 72, Author: "contributor", AuthorKind: authorKindClanker}},
		"unlisted repository": {s, StageTrackerItem{Kind: "pr", Repo: "acme/other", Number: 7, Stage: 71, Author: "someone", AuthorKind: authorKindHuman}},
		"not at 071 / 072":    {s, StageTrackerItem{Kind: "pr", Repo: "acme/widget", Number: 7, Stage: stagelabels.NeedsDecision.Number(), Author: "someone", AuthorKind: authorKindHuman}},
		"an issue":            {s, StageTrackerItem{Kind: "issue", Repo: "acme/widget", Number: 7, Stage: 71, Author: "someone", AuthorKind: authorKindHuman}},
	} {
		if pt := tc.s.stageTrackerPersonTakeover(tc.it, stagelabels.Record{}, false, now); pt != nil {
			t.Errorf("%s: person_takeover = %+v, want none", name, pt)
		}
	}
	below := personTakeoverServer("24h", "")
	level := 5
	below.deps.Config.ACMMLevel = &level
	if pt := below.stageTrackerPersonTakeover(human, stagelabels.Record{}, false, now); pt != nil {
		t.Errorf("below L6: person_takeover = %+v, want none", pt)
	}
}

// The 071 / 072 decision row stops saying the hive never rewrites a
// person's PR when the timer applies, and says it as before when it does not.
func TestDecisionForWaitingOnAuthorNamesTheTakeoverTimer(t *testing.T) {
	it := StageTrackerItem{Kind: "pr", Repo: "acme/widget", Number: 7, Stage: 71, Author: "someone", AuthorKind: authorKindHuman, Reason: "check-failing:unit"}
	if d := decisionForWaitingOnAuthor(it, false); !strings.Contains(d.Question, "The hive never rewrites a person's PR") {
		t.Fatalf("timer off: question = %q", d.Question)
	}
	it.PersonTakeover = &StageTrackerPersonTakeover{After: "24h", Grace: "24h", Text: "The hive posted its takeover notice and takes the PR over in 3h unless someone pushes, comments or adds hold."}
	d := decisionForWaitingOnAuthor(it, false)
	if strings.Contains(d.Question, "never rewrites") || !strings.Contains(d.Question, "takes the PR over in 3h") || !strings.HasPrefix(d.Question, "This PR waits on @someone to push") {
		t.Fatalf("timer on: question = %q", d.Question)
	}

	owner := StageTrackerItem{Kind: "pr", Repo: "acme/widget", Number: 8, Stage: 72, Author: "owner", AuthorKind: authorKindClanker,
		Adoption: &StageTrackerAdoption{Author: "owner", BlockedBy: stagelabels.AdoptBlockedAuthorPerson}}
	if d := decisionForWaitingOnContributor(owner, false, time.Now()); !strings.Contains(d.Question, "not adopting on its own") {
		t.Fatalf("owner agent PR, timer off: question = %q", d.Question)
	}
	owner.PersonTakeover = it.PersonTakeover
	if d := decisionForWaitingOnContributor(owner, false, time.Now()); strings.Contains(d.Question, "not adopting on its own") || !strings.Contains(d.Question, "takes the PR over in 3h") {
		t.Fatalf("owner agent PR, timer on: question = %q", d.Question)
	}
}
