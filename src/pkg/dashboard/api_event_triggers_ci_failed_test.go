package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/config"
)

// The Default-branch CI failed preset writes a one-repository ci.failed rule,
// like the first-seen presets; other kinds stay rejected.
func TestEventTriggersCIFailedPreset(t *testing.T) {
	for _, kind := range []string{celtrigger.KindIssueOpened, celtrigger.KindPROpened, celtrigger.KindCIFailed} {
		if !eventTriggerPresetKind(kind) {
			t.Errorf("preset kind %q rejected", kind)
		}
	}
	for _, kind := range []string{"", celtrigger.KindIssueLabeled, celtrigger.KindStageCompleted, celtrigger.KindCommentCreated} {
		if eventTriggerPresetKind(kind) {
			t.Errorf("preset kind %q accepted", kind)
		}
	}

	s := eventTriggerTestServer(t)
	cfg := s.deps.Config
	cfg.Triggers = nil
	get := doOwnerGet(s, "/api/config/triggers")
	if get.Code != http.StatusOK {
		t.Fatalf("get: %d %s", get.Code, get.Body.String())
	}
	index := 0
	body, err := json.Marshal(map[string]any{
		"rules":  []config.TriggerRule{{Name: "main-red", Agent: "scanner", Expr: "overwritten by scoped preset"}},
		"preset": eventTriggerPreset{Index: &index, Kind: celtrigger.KindCIFailed, Repo: "repo1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := putEventTriggers(s, string(body), get.Header().Get("ETag"))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	want := []config.TriggerRule{{Name: "main-red", Agent: "scanner", Expr: `event.kind == "ci.failed" && event.repo == "repo1"`}}
	if got := cfg.TriggerRulesSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("saved rules = %+v, want %+v", got, want)
	}
	engine, err := celtrigger.CompileFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ci := engine.ForKind(celtrigger.KindCIFailed)
	if got := ci.MatchAgents(celtrigger.NormalizedEvent{Kind: celtrigger.KindCIFailed, Repo: "repo1", Title: "build", HeadBranch: "main", State: "failure"}); !reflect.DeepEqual(got, []string{"scanner"}) {
		t.Fatalf("ci.failed preset did not match its repository: %v", got)
	}
	if got := ci.MatchAgents(celtrigger.NormalizedEvent{Kind: celtrigger.KindCIFailed, Repo: "other"}); len(got) != 0 {
		t.Fatalf("ci.failed preset broadened repository scope: %v", got)
	}
}
