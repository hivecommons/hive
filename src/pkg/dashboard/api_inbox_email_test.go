package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

func mailFilesServer(t *testing.T) (*Server, *inbox.EmailSource) {
	t.Helper()
	s, st := mailServer(t)
	proveFor(t, st, "bob", "bob@example.com")
	svc := inbox.NewService(config.InboxConfig{}, inbox.NewStore(filepath.Join(t.TempDir(), "inbox.json")), nil, nil, nil)
	src := &inbox.EmailSource{Dir: filepath.Join(t.TempDir(), "items")}
	svc.EnableEmail(src)
	s.deps.Inbox = svc
	return s, src
}

func filesDelivery(from string, aligned bool, files ...wire.Attachment) wire.Delivery {
	return wire.Delivery{Kind: wire.KindFiles, MessageID: "m-" + from, Recipient: "hive@acme.hive.hivecommons.dev",
		Auth: wire.Auth{From: from, Aligned: aligned}, Files: &wire.Files{Title: "notes", Attachments: files}}
}

func TestInboxEmailReceive(t *testing.T) {
	s, src := mailFilesServer(t)
	md := wire.Attachment{Name: "n.md", Kind: wire.AttachMarkdown, ContentType: "text/markdown", Data: []byte("# hi")}
	img := wire.Attachment{Name: "p.png", Kind: wire.AttachImage, ContentType: "image/png", Data: []byte("\x89PNG\r\n\x1a\nx")}

	rec := relayPost(s, inboxEmailPath, filesDelivery("bob@example.com", true, md), testMailRelayKey)
	var out struct {
		Items    []string `json:"items"`
		Awaiting bool     `json:"awaiting_approval"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || len(out.Items) != 1 || out.Awaiting {
		t.Fatalf("allowlisted sender: %d %s", rec.Code, rec.Body)
	}
	rec = relayPost(s, inboxEmailPath, filesDelivery("stranger@example.org", true, img), testMailRelayKey)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || !out.Awaiting {
		t.Fatalf("outsider: %d %s", rec.Code, rec.Body)
	}
	pendingID := out.Items[0]

	// Only the owner approves or declines.
	if rec := serve(s, userReq(http.MethodPost, "/api/inbox/email/"+pendingID+"/approve", "bob", "read-write", "{}")); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner approve: %d", rec.Code)
	}
	if rec := serve(s, userReq(http.MethodPost, "/api/inbox/email/"+pendingID+"/approve", "alice", "owner", "{}")); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"awaiting_approval":true`) {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(s, userReq(http.MethodPost, "/api/inbox/email/"+strings.Repeat("a", 24)+"/approve", "alice", "owner", "{}")); rec.Code != http.StatusNotFound {
		t.Fatalf("approve unknown: %d", rec.Code)
	}

	// The file is served with its sniffed type and no script.
	rec = serve(s, userReq(http.MethodGet, "/api/inbox/email/"+pendingID+"/file", "bob", "read-write", ""))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline") ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") || string(body) != string(img.Data) {
		t.Fatalf("file: %d %v", rec.Code, rec.Header())
	}
	mdItems, _ := src.List(t.Context())
	var mdID string
	for _, it := range mdItems {
		if it.UploadKind == wire.AttachMarkdown {
			mdID = it.ID
		}
	}
	rec = serve(s, userReq(http.MethodGet, "/api/inbox/email/"+mdID+"/file", "alice", "owner", ""))
	if rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("markdown file headers %v", rec.Header())
	}
	if rec := serve(s, userReq(http.MethodGet, "/api/inbox/email/"+mdID+"/file", "carol", "read", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("reader fetched a file: %d", rec.Code)
	}
	if rec := serve(s, userReq(http.MethodGet, "/api/inbox/email/zz/file", "alice", "owner", "")); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown file: %d", rec.Code)
	}

	if rec := serve(s, userReq(http.MethodPost, "/api/inbox/email/"+pendingID+"/decline", "alice", "owner", "{}")); rec.Code != http.StatusOK {
		t.Fatalf("decline: %d", rec.Code)
	}
	if rec := serve(s, userReq(http.MethodPost, "/api/inbox/email/"+pendingID+"/decline", "alice", "owner", "{}")); rec.Code != http.StatusNotFound {
		t.Fatalf("decline again: %d", rec.Code)
	}
	if rec := serve(s, userReq(http.MethodPost, "/api/inbox/email/"+pendingID+"/decline", "bob", "read-write", "{}")); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner decline: %d", rec.Code)
	}
}

func TestInboxEmailUnavailable(t *testing.T) {
	s, _ := mailFilesServer(t)
	src := &inbox.EmailSource{Dir: filepath.Join(t.TempDir(), "q"), QuotaBytes: 1}
	s.deps.Inbox.EnableEmail(src)
	big := wire.Attachment{Name: "n.md", Kind: wire.AttachMarkdown, Data: []byte("too big")}
	if rec := relayPost(s, inboxEmailPath, filesDelivery("bob@example.com", true, big), testMailRelayKey); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("quota: %d", rec.Code)
	}
	t.Setenv("HIVE_MEETINGS_EMAIL_FILES_ENABLED", "off")
	if rec := relayPost(s, inboxEmailPath, filesDelivery("bob@example.com", true, big), testMailRelayKey); rec.Code != http.StatusConflict {
		t.Fatalf("files off: %d", rec.Code)
	}
	t.Setenv("HIVE_MEETINGS_EMAIL_FILES_ENABLED", "")
	s.deps.Inbox.EnableEmail(nil)
	if rec := relayPost(s, inboxEmailPath, filesDelivery("bob@example.com", true, big), testMailRelayKey); rec.Code != http.StatusConflict {
		t.Fatalf("no source: %d", rec.Code)
	}
	for _, p := range []string{"/approve", "/decline"} {
		if rec := serve(s, userReq(http.MethodPost, "/api/inbox/email/"+strings.Repeat("a", 24)+p, "alice", "owner", "{}")); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without source: %d", p, rec.Code)
		}
	}
	if rec := serve(s, userReq(http.MethodGet, "/api/inbox/email/x/file", "alice", "owner", "")); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("file without source: %d", rec.Code)
	}
	// A blocked spool directory is a server error.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.deps.Inbox.EnableEmail(&inbox.EmailSource{Dir: filepath.Join(blocked, "items")})
	if rec := relayPost(s, inboxEmailPath, filesDelivery("bob@example.com", true, big), testMailRelayKey); rec.Code != http.StatusInternalServerError {
		t.Fatalf("blocked spool: %d", rec.Code)
	}
}
