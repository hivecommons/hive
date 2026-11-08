package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/maintainernotes"
)

// Per-item maintainer notes (owner request, multi-maintainer hives). A note
// is private context one maintainer leaves on an Activity-monitor row for
// the others — it is stored on the spoke (maintainernotes.DefaultPath),
// never posted to GitHub, shown to every signed-in maintainer of the hive
// and typed into agent kicks for the item (scheduler kick_maintainer_notes.go).
//
//   - GET    /api/notes?repo=owner/repo&number=N   the item's live notes;
//     without repo+number, every live note (the SPA's count badges)
//   - POST   /api/notes {repo, number, text}        append; author is the
//     session's GitHub login; needs write on the repo (canToggleRepoHold)
//   - DELETE /api/notes/{id}                        the author, or a verified owner
//
// Each write broadcasts a `maintainer-notes` SSE frame naming the item so
// other maintainers' browsers refresh the row live.
const (
	maintainerNotesSSEEvent = "maintainer-notes"
	maintainerNotesMaxBody  = 16 << 10

	auditMaintainerNoteAdd    = "maintainer_note_add"
	auditMaintainerNoteDelete = "maintainer_note_delete"
)

type maintainerNoteRequest struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Text   string `json:"text"`
}

type maintainerNotesResponse struct {
	OK    bool                   `json:"ok"`
	Notes []maintainernotes.Note `json:"notes"`
}

type maintainerNoteResponse struct {
	OK   bool                 `json:"ok"`
	Note maintainernotes.Note `json:"note"`
}

// maintainerNotesSSEPayload is the data of a `maintainer-notes` frame.
type maintainerNotesSSEPayload struct {
	Seq    uint64 `json:"seq"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Count  int    `json:"count"`
	Action string `json:"action"`
}

func (s *Server) maintainerNotesStore() *maintainernotes.Store {
	if s != nil && s.maintainerNotes != nil {
		return s.maintainerNotes
	}
	return maintainernotes.Default()
}

// maintainerNotesSession is the signed-in identity a notes request acts
// as: the GitHub login, and whether the session carries the verified owner
// role (which may read and delete everything).
func maintainerNotesSession(r *http.Request) (login string, owner bool) {
	owner = isOwnerRole(r.Header.Get("X-Hive-Role")) && r.Header.Get(ownerRoleVerifiedHeader) == "true"
	return strings.TrimSpace(r.Header.Get("X-Hive-User")), owner
}

func maintainerNotesSSEFrame(p maintainerNotesSSEPayload) string {
	data, _ := json.Marshal(p)
	return "event: " + maintainerNotesSSEEvent + "\ndata: " + string(data) + "\n\n"
}

func (s *Server) broadcastMaintainerNotes(repo string, number int, action string) {
	count := len(s.maintainerNotesStore().List(repo, number))
	s.broadcastFrame(maintainerNotesSSEFrame(maintainerNotesSSEPayload{
		Seq: s.maintainerNotesSeq.Add(1), Repo: repo, Number: number, Count: count, Action: action,
	}))
}

func (s *Server) handleMaintainerNotesList(w http.ResponseWriter, r *http.Request) {
	login, owner := maintainerNotesSession(r)
	if login == "" && !owner {
		jsonError(w, "GitHub user session required", http.StatusUnauthorized)
		return
	}
	store := s.maintainerNotesStore()
	repoParam := strings.TrimSpace(r.URL.Query().Get("repo"))
	numParam := strings.TrimSpace(r.URL.Query().Get("number"))
	if repoParam == "" && numParam == "" {
		notes := store.All()
		if notes == nil {
			notes = []maintainernotes.Note{}
		}
		jsonResponse(w, maintainerNotesResponse{OK: true, Notes: notes})
		return
	}
	repo, ok := maintainernotes.NormalizeRepo(repoParam)
	number, err := strconv.Atoi(numParam)
	if !ok || err != nil || number <= 0 {
		jsonError(w, "repo must be owner/repo and number a positive integer", http.StatusBadRequest)
		return
	}
	notes := store.List(repo, number)
	if notes == nil {
		notes = []maintainernotes.Note{}
	}
	jsonResponse(w, maintainerNotesResponse{OK: true, Notes: notes})
}

func (s *Server) handleMaintainerNotesAdd(w http.ResponseWriter, r *http.Request) {
	login, _ := maintainerNotesSession(r)
	if login == "" {
		jsonError(w, "GitHub user session required to leave a note", http.StatusUnauthorized)
		return
	}
	var body maintainerNoteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maintainerNotesMaxBody)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	repo, ok := maintainernotes.NormalizeRepo(body.Repo)
	if !ok || body.Number <= 0 {
		jsonError(w, "repo must be owner/repo and number a positive integer", http.StatusBadRequest)
		return
	}
	text, err := maintainernotes.ValidateText(body.Text)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	owner, name, _ := strings.Cut(repo, "/")
	if allowed, status, msg := s.canToggleRepoHold(r, owner, name); !allowed {
		jsonError(w, msg, status)
		return
	}
	note, err := s.maintainerNotesStore().Add(repo, body.Number, login, text)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.audit != nil {
		s.audit.LogRecord(login, auditMaintainerNoteAdd, auditDetail("repo", repo, "number", strconv.Itoa(body.Number), "id", note.ID), "", repo, body.Number)
	}
	s.broadcastMaintainerNotes(repo, body.Number, "added")
	jsonStatusResponse(w, http.StatusCreated, maintainerNoteResponse{OK: true, Note: note})
}

func (s *Server) handleMaintainerNotesDelete(w http.ResponseWriter, r *http.Request) {
	login, owner := maintainerNotesSession(r)
	if login == "" && !owner {
		jsonError(w, "GitHub user session required", http.StatusUnauthorized)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		jsonError(w, "note id required", http.StatusBadRequest)
		return
	}
	note, err := s.maintainerNotesStore().Delete(id, login, owner)
	switch {
	case errors.Is(err, maintainernotes.ErrNotFound):
		jsonError(w, "note not found", http.StatusNotFound)
		return
	case errors.Is(err, maintainernotes.ErrForbidden):
		jsonError(w, "only the note's author or an owner may delete it", http.StatusForbidden)
		return
	case err != nil:
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	actor := login
	if actor == "" {
		actor = "owner"
	}
	if s.audit != nil {
		s.audit.LogRecord(actor, auditMaintainerNoteDelete, auditDetail("repo", note.Repo, "number", strconv.Itoa(note.Number), "id", note.ID, "author", note.Author), "", note.Repo, note.Number)
	}
	s.broadcastMaintainerNotes(note.Repo, note.Number, "deleted")
	jsonResponse(w, maintainerNoteResponse{OK: true, Note: note})
}
