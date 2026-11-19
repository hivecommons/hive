package dashboard

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
)

func decodeEmailIn(t *testing.T, body []byte) *EmailInView {
	t.Helper()
	var out struct {
		EmailIn *EmailInView `json:"email_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	return out.EmailIn
}

// emailInServer is mailServer with an Inbox that has the email source and a
// scheduler that takes invites.
func emailInServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	withWebhookRelayKey(t, testMailRelayKey)
	s, deps := apiServer(t)
	deps.Config.Hub.DashboardURL = "https://acme.hive.hivecommons.dev/"
	deps.Inbox = newInboxTestService(t, deps.Config.Inbox)
	deps.Inbox.EnableEmail(&inbox.EmailSource{Dir: filepath.Join(t.TempDir(), "email", "items")})
	s.SetMeetingsService(&fakeMeetingsService{})
	s.SetMeetingsScheduleService(&fakeInviteScheduler{})
	return s, deps
}

// The three section endpoints carry the same address for the owner.
func TestEmailInAddressOnSectionEndpoints(t *testing.T) {
	s, _ := emailInServer(t)
	for _, path := range []string{"/api/inbox", "/api/meetings/schedule", "/api/meetings"} {
		rec := doGetRole(s, path, "owner", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
		}
		v := decodeEmailIn(t, rec.Body.Bytes())
		if v == nil || !v.Ready || v.Address != "hive@acme.hive.hivecommons.dev" || v.Reason != "" || !v.Files || !v.Invites || v.DocsURL != emailInDocsURL {
			t.Fatalf("%s email_in = %+v", path, v)
		}
	}
	// The section's own state is still there next to the address.
	var sched map[string]json.RawMessage
	if err := json.Unmarshal(doGetRole(s, "/api/meetings/schedule", "owner", true).Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"upcoming", "recent", "attendee_name", "email_in"} {
		if _, ok := sched[key]; !ok {
			t.Errorf("schedule state lost %q: %v", key, sched)
		}
	}
	var mtg struct {
		Meetings []map[string]string `json:"meetings"`
	}
	if err := json.Unmarshal(doGetRole(s, "/api/meetings", "owner", true).Body.Bytes(), &mtg); err != nil || len(mtg.Meetings) != 1 {
		t.Fatalf("meetings state = %+v, %v", mtg, err)
	}
}

// Owner and read-write see the address; read, anonymous and an unverified
// owner header never do.
func TestEmailInAddressRoleMatrix(t *testing.T) {
	s, _ := emailInServer(t)
	if v := decodeEmailIn(t, doGetRole(s, "/api/inbox", "read-write", false).Body.Bytes()); v == nil || v.Address == "" {
		t.Fatalf("read-write email_in = %+v", v)
	}
	if v := decodeEmailIn(t, doGetRole(s, "/api/meetings/schedule", "read-write", false).Body.Bytes()); v == nil || v.Address == "" {
		t.Fatalf("read-write calendar email_in = %+v", v)
	}
	for _, c := range []struct {
		role     string
		verified bool
	}{{"read", false}, {"", false}, {"owner", false}} {
		rec := doGetRole(s, "/api/inbox", c.role, c.verified)
		if rec.Code != http.StatusOK {
			t.Fatalf("role %q inbox = %d", c.role, rec.Code)
		}
		if v := decodeEmailIn(t, rec.Body.Bytes()); v != nil {
			t.Fatalf("role %q (verified=%v) must not get the address: %+v", c.role, c.verified, v)
		}
	}
}

// Without a hub relay key no mail can reach the hive: no address, a reason
// and the docs link instead.
func TestEmailInNotSetUp(t *testing.T) {
	withWebhookRelayKey(t, "")
	s, deps := apiServer(t)
	deps.Config.Hub.DashboardURL = "https://acme.hive.hivecommons.dev/"
	v := decodeEmailIn(t, doGetRole(s, "/api/inbox", "owner", true).Body.Bytes())
	if v == nil || v.Ready || v.Address != "" || v.Reason != emailInNoRelay || v.DocsURL != emailInDocsURL || v.Files || v.Invites {
		t.Fatalf("email_in without a relay key = %+v", v)
	}
}

// The switches and the wiring decide what each section may promise.
func TestEmailInSwitchesAndHost(t *testing.T) {
	s, deps := emailInServer(t)
	off := false
	deps.Config.Meetings.Email = config.MeetingsEmailConfig{InvitesEnabled: &off, FilesEnabled: &off}
	v := decodeEmailIn(t, doGetRole(s, "/api/inbox", "owner", true).Body.Bytes())
	if v == nil || !v.Ready || v.Files || v.Invites {
		t.Fatalf("switches off = %+v", v)
	}
	deps.Config.Meetings.Email = config.MeetingsEmailConfig{}
	// A scheduler that cannot take invites promises none.
	s.SetMeetingsScheduleService(&fakeScheduleService{})
	if v := decodeEmailIn(t, doGetRole(s, "/api/inbox", "owner", true).Body.Bytes()); v == nil || v.Invites || !v.Files {
		t.Fatalf("plain scheduler = %+v", v)
	}
	// No hub dashboard URL: the forwarded host, without its port.
	deps.Config.Hub.DashboardURL = ""
	rec := doGetRole(s, "/api/inbox", "owner", true)
	if v := decodeEmailIn(t, rec.Body.Bytes()); v == nil || v.Address != "hive@example.com" {
		t.Fatalf("request host fallback = %+v", v)
	}
}

func TestWithEmailInLeavesNonObjectStateAlone(t *testing.T) {
	s, _ := emailInServer(t)
	req := doGetRoleRequest("owner", true)
	if got, ok := s.withEmailIn(req, []int{1}).([]int); !ok || len(got) != 1 {
		t.Fatalf("a list state must pass through unchanged: %#v", got)
	}
	if got, ok := s.withEmailIn(req, make(chan int)).(chan int); !ok || got == nil {
		t.Fatal("an unencodable state must pass through unchanged")
	}
	if got, ok := s.withEmailIn(doGetRoleRequest("read", false), map[string]any{"a": 1}).(map[string]any); !ok || len(got) != 1 {
		t.Fatalf("a read caller's state must pass through unchanged: %#v", got)
	}
}

func doGetRoleRequest(role string, verified bool) *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/api/inbox", nil)
	req.Header.Set("X-Hive-Role", role)
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	return req
}
