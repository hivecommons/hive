package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
)

func inboxDeleteBody(ids ...string) map[string]any {
	items := []map[string]string{}
	for _, id := range ids {
		items = append(items, map[string]string{"source": "plaud", "item_id": id})
	}
	return map[string]any{"items": items}
}

// Delete is open to owner and read-write; read and anonymous get 403.
func TestInboxDeleteRoleMatrix(t *testing.T) {
	s, deps := apiServer(t)
	deps.Inbox = newInboxTestService(t, config.InboxConfig{})
	for _, role := range []string{"", "read-only", "read"} {
		if rec := doShareRolePost(s, "/api/inbox/delete", role, inboxDeleteBody("rec-1")); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q delete = %d, want 403", role, rec.Code)
		}
	}
	if items, _ := deps.Inbox.Items(t.Context()); len(items) != 1 {
		t.Fatalf("a refused delete removed the item: %+v", items)
	}
	rec := doShareRolePost(s, "/api/inbox/delete", "read-write", inboxDeleteBody("rec-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("read-write delete = %d: %s", rec.Code, rec.Body.String())
	}
}

// A synced item is tombstoned: gone from GET /api/inbox although its fact
// (which the sync owns) is still on disk, and the delete is audited.
func TestInboxDeleteSyncedItemTombstonedAndAudited(t *testing.T) {
	s, deps := apiServer(t)
	deps.Inbox = newInboxTestService(t, config.InboxConfig{})
	before := decodeInboxList(t, doGet(s, "/api/inbox").Body.Bytes())
	if len(before.Items) != 1 || before.Items[0].FactPath == "" {
		t.Fatalf("fixture = %+v", before.Items)
	}
	rec := doPost(s, "/api/inbox/delete", map[string]any{"items": []map[string]string{{"source": "plaud", "item_id": "rec-1"}}, "remove_knowledge": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	var resp InboxDeleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Deleted != 1 || len(resp.Items) != 1 || resp.Items[0].StoredRemoved || resp.Items[0].KnowledgeRemoved || !resp.Items[0].KnowledgeKept || len(resp.Failed) != 0 {
		t.Fatalf("response = %+v", resp)
	}
	if _, err := os.Stat(before.Items[0].FactPath); err != nil {
		t.Fatalf("a synced source's Knowledge fact must stay: %v", err)
	}
	if after := decodeInboxList(t, doGet(s, "/api/inbox").Body.Bytes()); len(after.Items) != 0 {
		t.Fatalf("deleted item is listed again: %+v", after.Items)
	}
	if s.audit != nil {
		found := false
		for _, e := range s.audit.Recent(50) {
			if e.Action == "inbox_delete" {
				found = true
			}
		}
		if !found {
			t.Error("no inbox_delete audit entry")
		}
	}
	// Deleting it again: nothing to delete, 404.
	if rec := doPost(s, "/api/inbox/delete", inboxDeleteBody("rec-1")); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
}

// One bad item does not stop a bulk delete; it is reported in failed.
func TestInboxDeleteBulkReportsFailures(t *testing.T) {
	s, deps := apiServer(t)
	deps.Inbox = newInboxTestService(t, config.InboxConfig{})
	rec := doPost(s, "/api/inbox/delete", inboxDeleteBody("rec-1", "no-such", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk delete = %d: %s", rec.Code, rec.Body.String())
	}
	var resp InboxDeleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Deleted != 1 || len(resp.Failed) != 1 || resp.Failed[0].ItemID != "no-such" || !strings.Contains(resp.Failed[0].Error, "no such inbox item") {
		t.Fatalf("response = %+v", resp)
	}
}

// An upload's stored file goes; its Knowledge fact goes only when asked.
func TestInboxDeleteUploadKnowledgeChoice(t *testing.T) {
	s, deps := apiServer(t)
	base := t.TempDir()
	deps.Inbox = newInboxTestService(t, config.InboxConfig{})
	deps.Inbox.EnableUploads(&inbox.UploadSource{Store: inbox.NewUploadStore(filepath.Join(base, "uploads")), ConnectorsDir: filepath.Join(base, "connectors")})
	add := func() inbox.Item {
		t.Helper()
		it, err := deps.Inbox.AddUpload(t.Context(), inbox.UploadInput{Filename: "notes.md", Data: []byte("# Plan\n\nShip it.\n"), AddedBy: "ana"})
		if err != nil || it.FactPath == "" {
			t.Fatalf("add upload: %+v, %v", it, err)
		}
		return it
	}
	kept := add()
	rec := doPost(s, "/api/inbox/delete", map[string]any{"items": []map[string]string{{"source": "upload", "item_id": kept.ID}}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"knowledge_kept":true`) || !strings.Contains(rec.Body.String(), `"stored_removed":true`) {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(kept.FactPath); err != nil {
		t.Fatalf("the Knowledge copy must stay by default: %v", err)
	}
	gone := add()
	rec = doPost(s, "/api/inbox/delete", map[string]any{"items": []map[string]string{{"source": "upload", "item_id": gone.ID}}, "remove_knowledge": true})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"knowledge_removed":true`) {
		t.Fatalf("delete with remove_knowledge = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(gone.FactPath); !os.IsNotExist(err) {
		t.Fatalf("the Knowledge copy must go when asked: %v", err)
	}
}

func TestInboxDeleteBadRequests(t *testing.T) {
	s, deps := apiServer(t)
	if rec := doPost(s, "/api/inbox/delete", inboxDeleteBody("rec-1")); rec.Code != http.StatusNotFound {
		t.Fatalf("delete without an inbox = %d, want 404", rec.Code)
	}
	deps.Inbox = newInboxTestService(t, config.InboxConfig{})
	if rec := doPost(s, "/api/inbox/delete", map[string]any{"items": []any{}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty delete = %d, want 400", rec.Code)
	}
	many := make([]string, maxInboxDeleteItems+1)
	for i := range many {
		many[i] = "x"
	}
	if rec := doPost(s, "/api/inbox/delete", inboxDeleteBody(many...)); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized delete = %d, want 400", rec.Code)
	}
}

func TestInboxDeleteStatus(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{inbox.ErrItemNotFound, http.StatusNotFound},
		{inbox.ErrItemOwnerOnly, http.StatusForbidden},
		{inbox.ErrRecordingNotYours, http.StatusForbidden},
		{inbox.ErrItemTranscribing, http.StatusConflict},
		{inbox.ErrRecordingNotFinished, http.StatusConflict},
		{errors.New("disk"), http.StatusInternalServerError},
	} {
		if got := inboxDeleteStatus(c.err); got != c.want {
			t.Errorf("inboxDeleteStatus(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
