package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
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

// TestInboxEmailDocumentsAndRecentMail (hivecommons/hive#18699): an office
// document mailed in becomes an Inbox item with the file attached, and the
// recent-mail log lists what was accepted and what was refused, with the
// reason and without any content.
func TestInboxEmailDocumentsAndRecentMail(t *testing.T) {
	s, src := mailFilesServer(t)
	const pptxType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	deck := wire.Attachment{Name: "KubeCon Talk.pptx", Kind: wire.AttachDocument, ContentType: pptxType, Data: []byte("PK\x03\x04deck")}

	d := filesDelivery("bob@example.com", true, deck)
	d.Files.Title = "Testing1-2-3"
	d.Files.Skipped = []wire.SkippedFile{{Ext: "key", Reason: wire.SkipType}}
	rec := relayPost(s, inboxEmailPath, d, testMailRelayKey)
	var out struct {
		Items []string `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || len(out.Items) != 1 {
		t.Fatalf("document delivery: %d %s", rec.Code, rec.Body)
	}
	items, _ := src.List(t.Context())
	if len(items) != 1 || items[0].UploadKind != wire.AttachDocument || items[0].Title != "Testing1-2-3" || items[0].AwaitingApproval {
		t.Fatalf("items %+v", items)
	}
	c, err := src.Fetch(t.Context(), items[0])
	if err != nil || !strings.Contains(c.Markdown, "A document, stored as an attachment") || !strings.Contains(c.Markdown, "KubeCon Talk.pptx") {
		t.Fatalf("content %v %q", err, c.Markdown)
	}
	// The file is a download with its stored type.
	rec = serve(s, userReq(http.MethodGet, "/api/inbox/email/"+out.Items[0]+"/file", "alice", "owner", ""))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != pptxType || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || string(body) != string(deck.Data) {
		t.Fatalf("file: %d %v", rec.Code, rec.Header())
	}

	// An outsider's mail awaits approval; a refusal report from the mail
	// receiver is recorded; so are this hive's own refusals.
	if rec := relayPost(s, inboxEmailPath, filesDelivery("stranger@example.org", true, deck), testMailRelayKey); rec.Code != http.StatusOK {
		t.Fatalf("outsider: %d %s", rec.Code, rec.Body)
	}
	// An allowlisted address whose mail failed authentication (sent through
	// a server its domain does not authorize) awaits approval too, and the
	// log says that this, not the allowlist, is why.
	spoofed := filesDelivery("bob@example.com", false, deck)
	spoofed.MessageID = "m-unauthenticated"
	spoofed.Auth.DMARC, spoofed.Auth.SPF, spoofed.Auth.SPFDomain = "fail", "softfail", "example.com"
	spoofed.Auth.RemoteIP, spoofed.Auth.HELO = "2001:db8::25", "Relay.Isp.Example"
	spoofed.Auth.DKIM = []wire.DKIMResult{{Domain: "isp.example", Result: "pass"}}
	if rec := relayPost(s, inboxEmailPath, spoofed, testMailRelayKey); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"awaiting_approval":true`) {
		t.Fatalf("unauthenticated allowlisted sender: %d %s", rec.Code, rec.Body)
	}
	report := wire.Delivery{Kind: wire.KindReport, Recipient: "hive@acme.hive.hivecommons.dev", Report: &wire.Report{
		Code: 550, Reason: wire.ReasonUnsupported, SenderDomain: "Example.NET", Bytes: 4096, Attachments: 1,
		Skipped: []wire.SkippedFile{{Ext: "key", Reason: wire.SkipType}, {Ext: "<b>x</b>", Reason: "made up reason"}},
	}}
	if rec := relayPost(s, inboxEmailReportPath, report, testMailRelayKey); rec.Code != http.StatusOK {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}
	if rec := relayPost(s, inboxEmailReportPath, report, "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("report with a bad signature: %d", rec.Code)
	}
	if rec := relayPost(s, inboxEmailReportPath, wire.Delivery{Kind: wire.KindReport}, testMailRelayKey); rec.Code != http.StatusBadRequest {
		t.Fatalf("report without a body: %d", rec.Code)
	}
	if rec := relayPost(s, inboxEmailReportPath, filesDelivery("bob@example.com", true, deck), testMailRelayKey); rec.Code != http.StatusBadRequest {
		t.Fatalf("a files delivery on the report path: %d", rec.Code)
	}
	t.Setenv("HIVE_MEETINGS_EMAIL_FILES_ENABLED", "off")
	if rec := relayPost(s, inboxEmailPath, filesDelivery("bob@example.com", true, deck), testMailRelayKey); rec.Code != http.StatusConflict {
		t.Fatalf("files off: %d", rec.Code)
	}
	t.Setenv("HIVE_MEETINGS_EMAIL_FILES_ENABLED", "")

	var listing struct {
		Events       []inbox.MailEvent `json:"events"`
		FilesEnabled bool              `json:"files_enabled"`
	}
	rec = serve(s, userReq(http.MethodGet, "/api/inbox/email/recent", "alice", "owner", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("recent: %d %s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	if err := json.Unmarshal([]byte(raw), &listing); err != nil || len(listing.Events) != 5 || !listing.FilesEnabled {
		t.Fatalf("recent: %v %s", err, raw)
	}
	// Newest first.
	off, refused, unauth, pending, accepted := listing.Events[0], listing.Events[1], listing.Events[2], listing.Events[3], listing.Events[4]
	if unauth.Outcome != inbox.MailAwaitingApproval || unauth.ApprovalReason != inbox.ApprovalNotAuthenticated || unauth.Auth == nil ||
		unauth.Auth.DMARC != "fail" || unauth.Auth.Aligned || unauth.Auth.SPF != "softfail" || unauth.Auth.SPFDomain != "example.com" ||
		unauth.Auth.RemoteIP != "2001:db8::25" || unauth.Auth.HELO != "relay.isp.example" || len(unauth.Auth.DKIM) != 1 || unauth.Auth.DKIM[0] != "isp.example:pass" {
		t.Errorf("unauthenticated event %+v %+v", unauth, unauth.Auth)
	}
	if off.Outcome != inbox.MailRejected || off.Reason != wire.ReasonMailInOff || off.SenderDomain != "example.com" {
		t.Errorf("files-off event %+v", off)
	}
	if refused.Outcome != inbox.MailRejected || refused.Reason != wire.ReasonUnsupported || refused.Code != 550 || refused.SenderDomain != "example.net" ||
		refused.Bytes != 4096 || refused.Files != 1 || len(refused.Skipped) != 2 || refused.Skipped[0] != (wire.SkippedFile{Ext: "key", Reason: wire.SkipType}) ||
		refused.Skipped[1] != (wire.SkippedFile{Ext: "bxb", Reason: "madeupreason"}) {
		t.Errorf("reported event %+v", refused)
	}
	if pending.Outcome != inbox.MailAwaitingApproval || pending.ApprovalReason != inbox.ApprovalNotOnAllowlist || pending.Auth == nil || !pending.Auth.Aligned ||
		pending.SenderDomain != "example.org" || pending.Files != 1 {
		t.Errorf("awaiting event %+v", pending)
	}
	if accepted.Outcome != inbox.MailAccepted || accepted.Kind != string(wire.KindFiles) || accepted.SenderDomain != "example.com" || accepted.Files != 1 ||
		accepted.Bytes != int64(len(deck.Data)) || len(accepted.Skipped) != 1 || accepted.At.IsZero() || accepted.ApprovalReason != "" {
		t.Errorf("accepted event %+v", accepted)
	}
	// No content: no subject, file name or full address.
	for _, private := range []string{"Testing1-2-3", "KubeCon", "bob@", "stranger@", "<b>"} {
		if strings.Contains(raw, private) {
			t.Errorf("the recent-mail log holds %q: %s", private, raw)
		}
	}
	mailHelpChecks(t, s, raw)
	if rec := serve(s, userReq(http.MethodGet, "/api/inbox/email/recent", "carol", "read", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("a reader listed recent mail: %d", rec.Code)
	}

	// A full spool is recorded as a refusal by the hive.
	full := &inbox.EmailSource{Dir: filepath.Join(t.TempDir(), "q"), QuotaBytes: 1}
	s.deps.Inbox.EnableEmail(full)
	if rec := relayPost(s, inboxEmailPath, filesDelivery("bob@example.com", true, deck), testMailRelayKey); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("quota: %d", rec.Code)
	}
	if evs := full.RecentMail(); len(evs) != 1 || evs[0].Outcome != inbox.MailRejected || evs[0].Reason != wire.ReasonHiveStorageFull || evs[0].Files != 1 {
		t.Fatalf("quota event %+v", evs)
	}

	// Without an email source the listing is empty, not an error.
	s.deps.Inbox.EnableEmail(nil)
	rec = serve(s, userReq(http.MethodGet, "/api/inbox/email/recent", "alice", "owner", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Fatalf("recent without a source: %d %s", rec.Code, rec.Body)
	}
}

// mailHelpChecks (hivecommons/hive#18744): each refused or held mail of the
// recent-mail list says how to try again, links the docs entry of its
// reason and a prefilled problem report that names no sender, and a held
// mail lists the items that still await approval until they are approved.
func mailHelpChecks(t *testing.T, s *Server, raw string) {
	t.Helper()
	type view struct {
		Outcome        string   `json:"outcome"`
		Reason         string   `json:"reason"`
		ApprovalReason string   `json:"approval_reason"`
		Items          []string `json:"items"`
		PendingItems   []string `json:"pending_items"`
		HowToFix       string   `json:"how_to_fix"`
		HelpURL        string   `json:"help_url"`
		ReportURL      string   `json:"report_url"`
	}
	list := func(raw string) []view {
		var listing struct {
			Events []view `json:"events"`
		}
		if err := json.Unmarshal([]byte(raw), &listing); err != nil {
			t.Fatalf("recent: %v %s", err, raw)
		}
		return listing.Events
	}
	var held []view
	for _, ev := range list(raw) {
		if ev.Outcome == inbox.MailAccepted {
			if ev.HowToFix != "" || ev.HelpURL != "" || ev.ReportURL != "" || len(ev.Items) != 0 {
				t.Errorf("a received mail carries help: %+v", ev)
			}
			continue
		}
		reason := ev.Reason
		if ev.Outcome == inbox.MailAwaitingApproval {
			reason = ev.ApprovalReason
			held = append(held, ev)
		}
		if ev.HowToFix != wire.Fix(reason) || ev.HowToFix == "" || ev.HelpURL != wire.HelpURL(reason) {
			t.Errorf("%s: fix %q help %q", reason, ev.HowToFix, ev.HelpURL)
		}
		u, err := url.Parse(ev.ReportURL)
		if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.Path != "/hivecommons/hive/issues/new" {
			t.Fatalf("%s: report url %q %v", reason, ev.ReportURL, err)
		}
		q := u.Query()
		body := q.Get("body")
		for _, want := range []string{"Reason code: `" + reason + "`", "Date: ", "Hive version: "} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the report lacks %q: %s", reason, want, body)
			}
		}
		if !strings.Contains(q.Get("title"), reason) || q.Get("labels") != "kind/bug,user-feedback" {
			t.Errorf("%s: title %q labels %q", reason, q.Get("title"), q.Get("labels"))
		}
		// No sender, not even the domain the list itself shows.
		for _, private := range []string{"example.com", "example.org", "example.net", "@", "isp.example", "2001:db8"} {
			if strings.Contains(body, private) || strings.Contains(q.Get("title"), private) {
				t.Errorf("%s: the report holds %q: %s", reason, private, body)
			}
		}
	}
	if len(held) != 2 {
		t.Fatalf("held events %+v", held)
	}
	for _, ev := range held {
		if len(ev.Items) != 1 || len(ev.PendingItems) != 1 || ev.PendingItems[0] != ev.Items[0] {
			t.Fatalf("held event items %+v", ev)
		}
	}
	// Approving one and declining the other empties both pending lists.
	if rec := serve(s, userReq(http.MethodPost, "/api/inbox/email/"+held[0].Items[0]+"/approve", "alice", "owner", "{}")); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(s, userReq(http.MethodPost, "/api/inbox/email/"+held[1].Items[0]+"/decline", "alice", "owner", "{}")); rec.Code != http.StatusOK {
		t.Fatalf("decline: %d %s", rec.Code, rec.Body)
	}
	rec := serve(s, userReq(http.MethodGet, "/api/inbox/email/recent", "alice", "owner", ""))
	for _, ev := range list(rec.Body.String()) {
		if len(ev.PendingItems) != 0 {
			t.Errorf("still pending after approve and decline: %+v", ev)
		}
	}
}
