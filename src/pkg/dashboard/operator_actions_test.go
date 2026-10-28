package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestOperatorActionsRoundTripCapAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actions.json")
	s := &Server{operatorActionPath: path}
	for i := 0; i < 25; i++ {
		entry := OperatorAction{Action: "hold", Actor: "operator", At: time.Unix(int64(i), 0).UTC(), Note: "note", Outcome: "ok", Head: "abc", Stage: 85}
		if err := s.appendOperatorAction("o/r", 7, entry); err != nil {
			t.Fatal(err)
		}
	}
	other := OperatorAction{Action: "release", Outcome: "failure"}
	if err := s.appendOperatorAction("o/r", 8, other); err != nil {
		t.Fatal(err)
	}
	restarted := &Server{operatorActionPath: path}
	entries, err := restarted.operatorActions("O/R", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != operatorActionCap || entries[0].At.Unix() != 24 || entries[19].At.Unix() != 5 {
		t.Fatalf("unexpected retained trail: %+v", entries)
	}
	if entries[0].Actor != "operator" || entries[0].Note != "note" || entries[0].Stage != 85 || entries[0].Head != "abc" {
		t.Fatalf("lost fields: %+v", entries[0])
	}
	entries[0].Note = "mutated"
	again, _ := restarted.operatorActions("o/r", 7)
	if again[0].Note != "note" {
		t.Fatal("caller mutated ledger")
	}
	separate, _ := restarted.operatorActions("o/r", 8)
	if !reflect.DeepEqual(separate, []OperatorAction{other}) {
		t.Fatalf("item isolation: %+v", separate)
	}
	rows := []StageTrackerItem{{Repo: "o/r", Number: 7}, {Repo: "o/r", Number: 9}}
	restarted.attachOperatorActions(rows)
	if rows[0].OperatorActionCount != 20 || rows[0].LastOperatorAction.At.Unix() != 24 || rows[1].LastOperatorAction != nil || rows[1].OperatorActionCount != 0 {
		t.Fatalf("tracker summary: %+v", rows)
	}
}

func TestOperatorActionsUnreadableLedgerIsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actions.json")
	bad := []byte("{broken")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{operatorActionPath: path}
	if err := s.appendOperatorAction("o/r", 1, OperatorAction{}); err == nil {
		t.Fatal("expected load error")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(bad) {
		t.Fatal("corrupt ledger overwritten")
	}
}

func TestRepoHoldRecordsSuccessFailureAndSurvivesLedgerFailure(t *testing.T) {
	s := newRepoHoldServer(t, &repoHoldMock{permission: map[string]string{"writer": "maintain"}})
	s.operatorActionPath = filepath.Join(t.TempDir(), "actions.json")
	if w := postRepoHold(t, s, "writer", true, true); w.Code != http.StatusOK {
		t.Fatalf("hold: %d %s", w.Code, w.Body.String())
	}
	entries, err := s.operatorActions("testorg/testrepo", 7)
	if err != nil || len(entries) != 1 || entries[0].Outcome != "ok" || entries[0].Action != "hold" || entries[0].Actor != "writer" {
		t.Fatalf("success: %+v %v", entries, err)
	}
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream failure", http.StatusNotFound)
	}))
	defer fail.Close()
	s.deps.GHClient = github.NewClient("token", "testorg", []string{"testrepo"}, s.logger, fail.URL)
	if w := postRepoHold(t, s, "writer", true, false); w.Code != http.StatusBadGateway {
		t.Fatalf("release: %d %s", w.Code, w.Body.String())
	}
	entries, err = s.operatorActions("testorg/testrepo", 7)
	if err != nil || len(entries) != 2 || entries[0].Outcome == "ok" || entries[0].Outcome == "" || entries[0].Action != "release" {
		t.Fatalf("failure: %+v %v", entries, err)
	}
	// Storage failure cannot turn a successful GitHub mutation into failure.
	s = newRepoHoldServer(t, &repoHoldMock{permission: map[string]string{"writer": "maintain"}})
	s.operatorActionPath = filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(s.operatorActionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if w := postRepoHold(t, s, "writer", true, true); w.Code != http.StatusOK {
		t.Fatalf("ledger failed action: %d %s", w.Code, w.Body.String())
	}
}

func TestRepoItemLedgerRouteNewestFirst(t *testing.T) {
	s := newFullServer(t)
	s.operatorActionPath = filepath.Join(t.TempDir(), "actions.json")
	for _, action := range []string{"hold", "release"} {
		if err := s.appendOperatorAction("testorg/testrepo", 7, OperatorAction{Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/repos/testorg/testrepo/items/7", nil))
	var entries []OperatorAction
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &entries) != nil || len(entries) != 2 || entries[0].Action != "release" || entries[1].Action != "hold" {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/repos/testorg/testrepo/items/0", "/api/repos/other/repo/items/7"} {
		w = httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code < 400 {
			t.Fatalf("invalid item accepted: %s", path)
		}
	}
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/repos/testorg/testrepo/items/99", nil))
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty trail: %s", w.Body.String())
	}
}

func TestRowRetryRecordsEverySelectedItem(t *testing.T) {
	s := newFullServer(t)
	s.operatorActionPath = filepath.Join(t.TempDir(), "actions.json")
	// An unavailable rescan is still a completed attempt, recorded per row.
	s.deps.RescanReposFunc = nil
	r := httptest.NewRequest(http.MethodPost, "/api/repos/rescan", strings.NewReader(`{"items":[{"repo":"testorg/testrepo","number":7},{"repo":"testorg/testrepo","number":8},{"repo":"testorg/testrepo","number":7}]}`))
	r.Header.Set("X-Hive-User", "operator")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("rescan: %d %s", w.Code, w.Body.String())
	}
	for _, number := range []int{7, 8} {
		entries, err := s.operatorActions("testorg/testrepo", number)
		if err != nil || len(entries) != 1 || entries[0].Action != "retry" || entries[0].Actor != "operator" || entries[0].Outcome == "ok" {
			t.Fatalf("retry #%d: %+v %v", number, entries, err)
		}
	}
}

func TestOperatorActionCapturesHeadAndStageBeforeMutation(t *testing.T) {
	s, _ := stageTrackerFixture(t, time.Now().UTC())
	s.operatorActionPath = filepath.Join(t.TempDir(), "actions.json")
	pr := s.status.Repos[0].OpenPrs[0].(FrontendPR)
	pr.HeadSHA = "before"
	s.status.Repos[0].OpenPrs[0] = pr
	entry := s.newOperatorAction("acme/console", 101, "operator", "decide:direction", "please revise")
	pr.HeadSHA = "after"
	s.status.Repos[0].OpenPrs[0] = pr
	s.recordOperatorAction("acme/console", 101, entry)
	entries, err := s.operatorActions("acme/console", 101)
	if err != nil || len(entries) != 1 || entries[0].Head != "before" || entries[0].Stage != trackerNumberReturned || entries[0].Note != "please revise" {
		t.Fatalf("snapshot: %+v %v", entries, err)
	}
}
