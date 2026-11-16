package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// TestMeetingAttendeeEmailSnapshot: once the allowlist is wired, the
// Settings tab's Email section is available and shows each user's calendar
// emails with proof status and the mail-in files waiting for the owner.
func TestMeetingAttendeeEmailSnapshot(t *testing.T) {
	s, src := mailFilesServer(t) // bob@example.com proven for bob
	if _, err := s.mailAllowlist().Add("alice", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Add(filesDelivery("stranger@example.org", false, wire.Attachment{Name: "x.md", Kind: wire.AttachMarkdown, Data: []byte("# x")}), false); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Add(filesDelivery("bob@example.com", true, wire.Attachment{Name: "y.md", Kind: wire.AttachMarkdown, Data: []byte("# y")}), true); err != nil {
		t.Fatal(err)
	}
	got := personaGet(t, s).Email
	if !got.Available || !got.CalendarInvites || !got.MailInFiles {
		t.Fatalf("email view %+v", got)
	}
	byLogin := map[string][]MeetingsCalendarEmail{}
	for _, p := range got.Inviters {
		byLogin[p.Login] = p.Emails
	}
	if b := byLogin["bob"]; len(b) != 1 || b[0].Proof != mailProofVerified || b[0].VerifiedAt == "" {
		t.Fatalf("bob %+v", b)
	}
	if a := byLogin["alice"]; len(a) != 1 || a[0].Proof != mailProofPending {
		t.Fatalf("alice %+v", a)
	}
	if len(got.Approvals) != 1 || got.Approvals[0].From != "stranger@example.org" || got.Approvals[0].Kind != mailApprovalKindFile || got.Approvals[0].ReceivedAt == "" {
		t.Fatalf("approvals %+v", got.Approvals)
	}

	// Without an allowlist the section is pending again.
	s.SetMailAllowlist(nil)
	if personaGet(t, s).Email.Available {
		t.Fatal("email available without the allowlist")
	}
}

func TestMeetingAttendeeEmailSnapshotWithoutInbox(t *testing.T) {
	s, _ := mailServer(t)
	snap, err := (mailAttendeeEmail{s: s}).Snapshot(t.Context())
	if err != nil || len(snap.Approvals) != 0 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	s.SetMailAllowlist(nil)
	if snap, _ := (mailAttendeeEmail{s: s}).Snapshot(t.Context()); len(snap.Emails) != 0 {
		t.Fatal("emails without a store")
	}
}

// TestMeetingAttendeeEmailSwitches: the owner turns invites and mail-in
// files off from Settings; a read-write user cannot.
func TestMeetingAttendeeEmailSwitches(t *testing.T) {
	t.Setenv("HIVE_MEETINGS_EMAIL_INVITES_ENABLED", "")
	t.Setenv("HIVE_MEETINGS_EMAIL_FILES_ENABLED", "")
	s, _ := mailServer(t)
	body := `{"email":{"calendar_invites":false,"mail_in_files":false}}`
	if rec := personaReq(s, http.MethodPut, "/api/meetings/persona", body, "read-write", false); rec.Code != http.StatusForbidden {
		t.Fatalf("read-write switch: %d", rec.Code)
	}
	rec := personaReq(s, http.MethodPut, "/api/meetings/persona", body, "owner", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner switch: %d %s", rec.Code, rec.Body)
	}
	var got MeetingsPersonaSettings
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Email.CalendarInvites || got.Email.MailInFiles {
		t.Fatalf("switches not saved: %+v", got.Email)
	}
	if s.meetingsEmailConfig().InvitesOn() || s.meetingsEmailConfig().FilesOn() {
		t.Fatal("config not updated")
	}
	// A persona save without the email block leaves them alone.
	personaPut(t, s, `{"name":"Betty"}`, http.StatusOK)
	if s.meetingsEmailConfig().InvitesOn() {
		t.Fatal("persona save reset the switches")
	}
	personaReq(s, http.MethodPut, "/api/meetings/persona", `{"email":{"calendar_invites":true}}`, "owner", true)
	if !s.meetingsEmailConfig().InvitesOn() || s.meetingsEmailConfig().FilesOn() {
		t.Fatal("a partial email block changes only what it names")
	}
}
