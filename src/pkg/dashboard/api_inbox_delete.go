package dashboard

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/hivecommons/hive/pkg/inbox"
)

// Deleting Inbox items (hivecommons/hive#18694). POST /api/inbox/delete
// deletes one or many items of any kind; pkg/inbox/delete.go says what goes
// with each kind. Owner or read-write, like the other Inbox actions that
// change an item; every deleted item is audited as inbox_delete.

func (s *Server) registerInboxDeleteRoutes() {
	s.mux.HandleFunc("POST /api/inbox/delete", s.handleInboxDelete)
}

// maxInboxDeleteItems bounds one bulk delete.
const maxInboxDeleteItems = 500

type inboxDeleteRequest struct {
	Items []struct {
		Source string `json:"source"`
		ItemID string `json:"item_id"`
	} `json:"items"`
	// RemoveKnowledge also deletes the Knowledge fact of items whose fact
	// this hive wrote itself (uploads, hive recordings). Default: kept.
	RemoveKnowledge bool `json:"remove_knowledge,omitempty"`
}

// InboxDeleteFailure is one item a bulk delete could not delete.
type InboxDeleteFailure struct {
	Source string `json:"source"`
	ItemID string `json:"item_id"`
	Error  string `json:"error"`
}

// InboxDeleteResponse is POST /api/inbox/delete.
type InboxDeleteResponse struct {
	Deleted int                 `json:"deleted"`
	Items   []inbox.ItemDeleted `json:"items"`
	// Failed lists the items that were not deleted, each with its reason.
	Failed []InboxDeleteFailure `json:"failed,omitempty"`
}

// inboxDeleteStatus is the status of a delete in which nothing was deleted.
func inboxDeleteStatus(err error) int {
	switch {
	case errors.Is(err, inbox.ErrItemNotFound):
		return http.StatusNotFound
	case errors.Is(err, inbox.ErrItemOwnerOnly), errors.Is(err, inbox.ErrRecordingNotYours):
		return http.StatusForbidden
	case errors.Is(err, inbox.ErrItemTranscribing), errors.Is(err, inbox.ErrRecordingNotFinished):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// handleInboxDelete (owner or read-write) deletes Inbox items for good. An
// item that cannot be deleted (being transcribed, someone else's recording)
// is reported in `failed` and does not stop the others; when nothing was
// deleted the first failure decides the status.
func (s *Server) handleInboxDelete(w http.ResponseWriter, r *http.Request) {
	if !requireInboxShareRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	var req inboxDeleteRequest
	if err := decodeBody(r, &req); err != nil || len(req.Items) == 0 {
		jsonError(w, "items[] is required", http.StatusBadRequest)
		return
	}
	if len(req.Items) > maxInboxDeleteItems {
		jsonError(w, "too many items in one delete (at most "+strconv.Itoa(maxInboxDeleteItems)+")", http.StatusBadRequest)
		return
	}
	actor := requestUser(r)
	opts := inbox.DeleteOptions{
		Actor: actor, AsOwner: isOwnerRole(r.Header.Get("X-Hive-Role")),
		RemoveKnowledge: req.RemoveKnowledge, Recording: s.inboxRecordingOptions(r),
	}
	resp := InboxDeleteResponse{Items: []inbox.ItemDeleted{}}
	var firstErr error
	for _, it := range req.Items {
		if it.Source == "" || it.ItemID == "" {
			continue
		}
		res, err := svc.DeleteItem(r.Context(), it.Source, it.ItemID, opts)
		if err != nil && (errors.Is(err, inbox.ErrItemNotFound) || !svc.Store.Deleted(inbox.Key(it.Source, it.ItemID))) {
			if firstErr == nil {
				firstErr = err
			}
			resp.Failed = append(resp.Failed, InboxDeleteFailure{Source: it.Source, ItemID: it.ItemID, Error: err.Error()})
			continue
		}
		if err != nil {
			// The item is gone from the Inbox; a leftover (a fact, a
			// transcript record) is logged, not reported as a failure.
			s.logger.Warn("inbox: delete left something behind", "item", inbox.Key(it.Source, it.ItemID), "error", err)
		}
		resp.Items = append(resp.Items, res)
		s.auditFromRequest(r, "inbox_delete", auditDetail("source", it.Source, "item", it.ItemID, "actor", actor,
			"stored_removed", strconv.FormatBool(res.StoredRemoved), "knowledge_removed", strconv.FormatBool(res.KnowledgeRemoved),
			"knowledge_kept", strconv.FormatBool(res.KnowledgeKept), "shares_kept", strconv.Itoa(res.SharesKept)), "")
	}
	resp.Deleted = len(resp.Items)
	w.Header().Set("Cache-Control", "no-store")
	if resp.Deleted == 0 && firstErr != nil {
		jsonError(w, firstErr.Error(), inboxDeleteStatus(firstErr))
		return
	}
	jsonResponse(w, resp)
}
