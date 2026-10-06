package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/escalation"
)

func TestClearReleasedReporterTrustNeedsHuman(t *testing.T) {
	for _, tc := range []struct {
		name                                                                                                                              string
		escalated, held, noNotice, forged, appRemoval, botRemoval, noRemoval, laterReason, earlierReason, eventsFail, deleteFail, noCheck bool
		wantClear                                                                                                                         bool
	}{
		{name: "human release", wantClear: true},
		{name: "escalated PR retains label", escalated: true},
		{name: "still held", held: true},
		{name: "no trust notice", noNotice: true},
		{name: "forged notice", forged: true},
		{name: "App removal", appRemoval: true},
		{name: "another bot removal", botRemoval: true},
		{name: "no removal evidence", noRemoval: true},
		{name: "later independent label", laterReason: true},
		{name: "preexisting independent label", earlierReason: true},
		{name: "events unavailable", eventsFail: true},
		{name: "write failed", deleteFail: true},
		{name: "missing reason check fails closed", noCheck: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/o/r/issues/11/comments":
					if tc.noNotice {
						fmt.Fprint(w, "[]")
						return
					}
					author := testHiveAppBotLogin
					if tc.forged {
						author = "stranger"
					}
					json.NewEncoder(w).Encode([]map[string]any{{"body": ReporterTrustNoticeMarker, "created_at": "2026-10-01T12:00:01Z", "user": map[string]string{"login": author}}})
				case "/repos/o/r/issues/11/events":
					if tc.eventsFail {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					event := func(label, kind, login, userType, at string) map[string]any {
						return map[string]any{"label": map[string]string{"name": label}, "event": kind, "actor": map[string]string{"login": login, "type": userType}, "created_at": at}
					}
					// Put the release on a second page: the scanner must read all pages.
					if r.URL.Query().Get("page") != "2" {
						w.Header().Set("Link", "<"+"http://"+r.Host+r.URL.Path+"?page=2>; rel=\"next\"")
						at := "2026-10-01T12:00:00Z"
						if tc.earlierReason {
							at = "2026-10-01T11:00:00Z"
						}
						json.NewEncoder(w).Encode([]map[string]any{
							event("hold", "labeled", testHiveAppBotLogin, "Bot", "2026-10-01T12:00:00Z"),
							event("needs-human", "labeled", testHiveAppBotLogin, "Bot", at),
						})
						return
					}
					events := []map[string]any{}
					if !tc.noRemoval {
						actor, kind := "maintainer", "User"
						if tc.appRemoval {
							actor, kind = testHiveAppBotLogin, "Bot"
						}
						if tc.botRemoval {
							actor, kind = "other[bot]", "Bot"
						}
						events = append(events, event("hold", "unlabeled", actor, kind, "2026-10-01T12:02:00Z"))
					}
					if tc.laterReason {
						events = append(events, event("needs-human", "labeled", testHiveAppBotLogin, "Bot", "2026-10-01T12:01:00Z"))
					}
					json.NewEncoder(w).Encode(events)
				case "/repos/o/r/issues/11/labels/needs-human":
					if r.Method != http.MethodDelete {
						t.Errorf("method = %s", r.Method)
					}
					removes++
					if tc.deleteFail {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					fmt.Fprint(w, "[]")
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := NewClientForTest(srv.URL, "o/r", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			c.SetAppBotLogin(testHiveAppBotLogin)
			store := escalation.Load(filepath.Join(t.TempDir(), "ledger.json"))
			if tc.escalated {
				store.ObserveRed([]escalation.Observation{{Repo: "o/r", Number: 11, HeadSHA: "red", Red: true}})
				store.MarkEscalated("o/r", 11)
				if !store.IsEscalated("o/r", 11) {
					t.Fatal("fixture not escalated")
				}
			}
			if !tc.noCheck {
				c.SetOtherNeedsHumanReason(store.IsEscalated)
			}
			labels := []string{"needs-human", "bug"}
			if tc.held {
				labels = append(labels, "hold")
			}
			got := c.clearReleasedReporterTrustNeedsHuman(context.Background(), "o/r", 11, labels)
			if cleared := !hasExactLabel(got, "needs-human"); cleared != tc.wantClear {
				t.Fatalf("labels = %v, want clear %v", got, tc.wantClear)
			}
			if !hasExactLabel(got, "bug") {
				t.Fatal("unrelated label removed")
			}
			wantRemoves := 0
			if tc.wantClear || tc.deleteFail {
				wantRemoves = 1
			}
			if removes != wantRemoves {
				t.Fatalf("removes = %d, want %d", removes, wantRemoves)
			}
		})
	}
}
