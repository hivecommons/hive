package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// projectbluefin/common#639: a maintainer closed the advisory digest
// ("no longer used") on 2026-10-27 and the hive reopened it the next day. A
// person's close is honoured: the issue stays closed and a new advisory issue
// says why. A close by the hive or a bot, or an unknown closer, is reused
// (#4167).
func advisoryPersonCloseMux(t *testing.T, closedBy map[string]any, created *string, reopened *atomic.Int64) *http.ServeMux {
	t.Helper()
	org, repo, n := "testorg", "testrepo", 639
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []any{}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/labels", org, repo, n), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"name": advisoryLabelName}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d", org, repo, n), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			reopened.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "title": advisoryTitle, "state": "open"})
			return
		}
		issue := map[string]any{"number": n, "title": advisoryTitle, "state": "closed", "closed_at": "2026-10-27T20:51:28Z"}
		if closedBy != nil {
			issue["closed_by"] = closedBy
		}
		_ = json.NewEncoder(w).Encode(issue)
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			*created = body.Body
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 999, "title": advisoryTitle})
			return
		}
		if r.URL.Query().Get("state") == "closed" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": n, "title": advisoryTitle, "state": "closed"}})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	return mux
}

func TestEnsureAdvisoryIssue_HonoursPersonClose(t *testing.T) {
	for _, tc := range []struct {
		name     string
		closedBy map[string]any
		reuse    bool
	}{
		{name: "a maintainer closed it", closedBy: map[string]any{"login": "Danathar", "type": "User"}, reuse: false},
		{name: "the hive closed it", closedBy: map[string]any{"login": "hive[bot]", "type": "Bot"}, reuse: true},
		{name: "another bot closed it", closedBy: map[string]any{"login": "stale[bot]", "type": "Bot"}, reuse: true},
		{name: "GitHub names no closer", closedBy: nil, reuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var created string
			var reopened atomic.Int64
			srv := httptest.NewServer(advisoryPersonCloseMux(t, tc.closedBy, &created, &reopened))
			defer srv.Close()
			c := newTestClient(t, srv, "testorg", []string{"testrepo"})
			c.SetAppBotLogin("hive[bot]")
			num, err := c.EnsureAdvisoryIssue(context.Background(), "testrepo")
			if err != nil {
				t.Fatalf("EnsureAdvisoryIssue: %v", err)
			}
			if tc.reuse {
				if num != 639 || reopened.Load() != 1 || created != "" {
					t.Fatalf("num=%d reopened=%d created=%q; want #639 reopened, nothing created", num, reopened.Load(), created)
				}
				return
			}
			if reopened.Load() != 0 {
				t.Fatal("an advisory issue a person closed must not be reopened")
			}
			if num != 999 {
				t.Fatalf("num = %d, want the new issue 999", num)
			}
			for _, want := range []string{"This replaces #639, which Danathar closed on 2026-10-27", "does not reopen an issue a person closed", "`advisory.target: linear`", "reopen it and close this one"} {
				if !strings.Contains(created, want) {
					t.Errorf("new advisory body lacks %q:\n%s", want, created)
				}
			}
			if strings.Contains(created, "@Danathar") {
				t.Errorf("the body must not ping the person who closed it:\n%s", created)
			}
		})
	}
}

// The ForgeState path (a memoized number) honours a person's close too: the
// cached issue is not reopened and the lookup falls through to create.
func TestFindAdvisoryIssue_CachedPersonCloseNotReopened(t *testing.T) {
	var created string
	var reopened atomic.Int64
	srv := httptest.NewServer(advisoryPersonCloseMux(t, map[string]any{"login": "Danathar", "type": "User"}, &created, &reopened))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "testorg", []string{"testrepo"})
	c.SetAppBotLogin("hive[bot]")
	store := &fakeForgeState{snapOK: true, snap: forgestate.Snapshot{Repo: "testorg/testrepo", Ready: true}, readOK: false}
	useAdvisoryForgeState(t, store)
	c.rememberAdvisoryIssue("testorg/testrepo", 639)

	num, err := c.EnsureAdvisoryIssue(context.Background(), "testrepo")
	if err != nil || num != 999 {
		t.Fatalf("EnsureAdvisoryIssue = %d, %v; want the new issue 999", num, err)
	}
	if reopened.Load() != 0 || len(store.mutations) != 0 {
		t.Fatalf("person-closed cached issue reopened: reopens=%d mutations=%+v", reopened.Load(), store.mutations)
	}
	if !strings.Contains(created, "This replaces #639") {
		t.Fatalf("new advisory body must say what it replaces:\n%s", created)
	}
	if _, ok := c.takeAdvisoryPersonClose("testorg/testrepo"); ok {
		t.Fatal("the note is consumed by the create")
	}
}
