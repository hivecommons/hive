package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/maintainernotes"
)

func newMaintainerNotesServer(t *testing.T) (*Server, chan []byte) {
	t.Helper()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collaborators/writer/permission"):
			_, _ = io.WriteString(w, `{"permission":"write"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collaborators/reader/permission"):
			_, _ = io.WriteString(w, `{"permission":"read"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ghSrv.Close)
	srv := newFullServer(t)
	srv.deps.GHClient = github.NewClient("token", "testorg", []string{"testrepo"}, slog.Default(), ghSrv.URL)
	srv.maintainerNotes = maintainernotes.New("")
	frames := make(chan []byte, 16)
	srv.sseMu.Lock()
	srv.sseClients[frames] = struct{}{}
	srv.sseMu.Unlock()
	return srv, frames
}

func maintainerNotesDo(srv *Server, method, target, user, body string, owner bool) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if owner {
		req.Header.Set("X-Hive-Role", "owner")
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestMaintainerNotesAPIAuthAndValidation(t *testing.T) {
	srv, _ := newMaintainerNotesServer(t)
	const body = `{"repo":"testorg/testrepo","number":7,"text":"waiting on legal"}`

	if w := maintainerNotesDo(srv, http.MethodGet, "/api/notes?repo=testorg/testrepo&number=7", "", "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: code=%d want 401: %s", w.Code, w.Body)
	}
	if w := maintainerNotesDo(srv, http.MethodPost, "/api/notes", "", body, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous add: code=%d want 401: %s", w.Code, w.Body)
	}
	if w := maintainerNotesDo(srv, http.MethodPost, "/api/notes", "reader", body, false); w.Code != http.StatusForbidden {
		t.Fatalf("reader add: code=%d want 403: %s", w.Code, w.Body)
	}
	for name, bad := range map[string]string{
		"no slash":  `{"repo":"testorg","number":7,"text":"x"}`,
		"number 0":  `{"repo":"testorg/testrepo","number":0,"text":"x"}`,
		"empty":     `{"repo":"testorg/testrepo","number":7,"text":"   "}`,
		"too long":  `{"repo":"testorg/testrepo","number":7,"text":"` + strings.Repeat("a", maintainernotes.MaxTextLen+1) + `"}`,
		"not json":  `{`,
		"bad query": "",
	} {
		var w *httptest.ResponseRecorder
		if name == "bad query" {
			w = maintainerNotesDo(srv, http.MethodGet, "/api/notes?repo=testorg&number=x", "writer", "", false)
		} else {
			w = maintainerNotesDo(srv, http.MethodPost, "/api/notes", "writer", bad, false)
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d want 400: %s", name, w.Code, w.Body)
		}
	}
	if got := len(srv.maintainerNotes.All()); got != 0 {
		t.Fatalf("rejected requests must store nothing, have %d", got)
	}
}

func TestMaintainerNotesAPIAddListDeleteBroadcast(t *testing.T) {
	srv, frames := newMaintainerNotesServer(t)

	w := maintainerNotesDo(srv, http.MethodPost, "/api/notes", "writer", `{"repo":"TestOrg/TestRepo","number":7,"text":"owner wants this after v6"}`, false)
	if w.Code != http.StatusCreated {
		t.Fatalf("add: code=%d: %s", w.Code, w.Body)
	}
	var added maintainerNoteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil || !added.OK || added.Note.ID == "" {
		t.Fatalf("add response: %v %s", err, w.Body)
	}
	if added.Note.Author != "writer" || added.Note.Repo != "testorg/testrepo" {
		t.Fatalf("author/repo must come from the session and be normalised: %+v", added.Note)
	}

	select {
	case f := <-frames:
		s := string(f)
		if !strings.HasPrefix(s, "event: "+maintainerNotesSSEEvent+"\n") || !strings.Contains(s, `"repo":"testorg/testrepo"`) || !strings.Contains(s, `"number":7`) || !strings.Contains(s, `"count":1`) || !strings.Contains(s, `"action":"added"`) {
			t.Fatalf("add frame = %q", s)
		}
	default:
		t.Fatal("add must broadcast a maintainer-notes SSE frame")
	}

	// Any signed-in maintainer reads the list, including the un-filtered
	// one the badges use.
	w = maintainerNotesDo(srv, http.MethodGet, "/api/notes?repo=testorg/testrepo&number=7", "reader", "", false)
	var list maintainerNotesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK || len(list.Notes) != 1 || list.Notes[0].Text != "owner wants this after v6" {
		t.Fatalf("list: code=%d err=%v body=%s", w.Code, err, w.Body)
	}
	w = maintainerNotesDo(srv, http.MethodGet, "/api/notes", "reader", "", false)
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Notes) != 1 {
		t.Fatalf("list all: err=%v body=%s", err, w.Body)
	}

	// Delete: a different login is refused, the author (or an owner) succeeds.
	if w := maintainerNotesDo(srv, http.MethodDelete, "/api/notes/"+added.Note.ID, "reader", "", false); w.Code != http.StatusForbidden {
		t.Fatalf("other user delete: code=%d want 403: %s", w.Code, w.Body)
	}
	if w := maintainerNotesDo(srv, http.MethodDelete, "/api/notes/"+added.Note.ID, "writer", "", false); w.Code != http.StatusOK {
		t.Fatalf("author delete: code=%d: %s", w.Code, w.Body)
	}
	select {
	case f := <-frames:
		if s := string(f); !strings.Contains(s, `"count":0`) || !strings.Contains(s, `"action":"deleted"`) {
			t.Fatalf("delete frame = %q", s)
		}
	default:
		t.Fatal("delete must broadcast a maintainer-notes SSE frame")
	}
	if w := maintainerNotesDo(srv, http.MethodDelete, "/api/notes/"+added.Note.ID, "", "", true); w.Code != http.StatusNotFound {
		t.Fatalf("deleted twice: code=%d want 404: %s", w.Code, w.Body)
	}

	// An owner-role session deletes another author's note.
	w = maintainerNotesDo(srv, http.MethodPost, "/api/notes", "writer", `{"repo":"testorg/testrepo","number":8,"text":"second"}`, false)
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("add #2: code=%d err=%v", w.Code, err)
	}
	<-frames
	if w := maintainerNotesDo(srv, http.MethodDelete, "/api/notes/"+added.Note.ID, "", "", true); w.Code != http.StatusOK {
		t.Fatalf("owner delete: code=%d: %s", w.Code, w.Body)
	}
	actions := map[string]bool{}
	for _, e := range srv.GetAudit().Recent(20) {
		actions[e.Action] = true
	}
	if !actions[auditMaintainerNoteAdd] || !actions[auditMaintainerNoteDelete] {
		t.Fatalf("audit must record add and delete, got %v", actions)
	}
}
