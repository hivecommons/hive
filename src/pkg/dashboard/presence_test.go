package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestHandlePresenceEngaged covers the focus-aware presence beacon: an engaged
// ping from an identified user lands them in the engaged set; everything else
// — not-engaged, unidentified, malformed — is a silent no-op that never
// invents engagement.
func TestHandlePresenceEngaged(t *testing.T) {
	cases := []struct {
		name        string
		user        string
		body        string
		wantEngaged bool
	}{
		{"engaged ping from identified user", "alice", `{"engaged":true}`, true},
		{"engaged:false never marks", "alice", `{"engaged":false}`, false},
		{"no identity header is a no-op", "", `{"engaged":true}`, false},
		{"malformed body reads as not engaged", "alice", `{{{`, false},
		{"empty body reads as not engaged", "alice", ``, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			req := httptest.NewRequest("POST", "/api/presence", strings.NewReader(tc.body))
			if tc.user != "" {
				req.Header.Set("X-Hive-User", tc.user)
			}
			rec := httptest.NewRecorder()
			s.handlePresence(rec, req)
			if rec.Code != 204 {
				t.Fatalf("presence beacon must always 204, got %d", rec.Code)
			}
			engaged := s.EngagedSessionUsernames()
			got := len(engaged) == 1 && engaged[0] == tc.user
			if got != tc.wantEngaged {
				t.Errorf("engaged set = %v, wantEngaged=%v", engaged, tc.wantEngaged)
			}
		})
	}
}

func TestHandlePresenceSnapshotAuthenticated(t *testing.T) {
	s := newTestServer()
	s.markUserEngaged("alice", time.Now())
	s.createUserSession("alice", "owner")
	s.createUserSession("bob", "read")
	s.audit.Log("bob", "config_save", "", "")

	req := httptest.NewRequest("GET", "/api/presence", nil)
	req.Header.Set("X-Hive-User", "alice")
	rec := httptest.NewRecorder()
	s.handlePresenceSnapshot(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /api/presence = %d, want 200", rec.Code)
	}
	var body struct {
		Mode  string `json:"mode"`
		Users []struct {
			Username   string `json:"username"`
			AvatarURL  string `json:"avatar_url"`
			Active     bool   `json:"active"`
			Idle       bool   `json:"idle"`
			LastAction string `json:"last_action"`
			You        bool   `json:"you"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode presence: %v", err)
	}
	if body.Mode != "authenticated" || len(body.Users) != 2 {
		t.Fatalf("presence body = %+v, want authenticated alice+bob", body)
	}
	byUser := map[string]struct {
		AvatarURL  string
		Active     bool
		Idle       bool
		LastAction string
		You        bool
	}{}
	for _, u := range body.Users {
		byUser[u.Username] = struct {
			AvatarURL  string
			Active     bool
			Idle       bool
			LastAction string
			You        bool
		}{u.AvatarURL, u.Active, u.Idle, u.LastAction, u.You}
	}
	if !byUser["alice"].Active || byUser["alice"].Idle || !byUser["alice"].You {
		t.Errorf("alice presence = %+v, want active/current viewer", byUser["alice"])
	}
	if !byUser["bob"].Idle || byUser["bob"].Active || byUser["bob"].LastAction == "" {
		t.Errorf("bob presence = %+v, want idle with last action", byUser["bob"])
	}
	if byUser["alice"].AvatarURL != "https://github.com/alice.png" {
		t.Errorf("avatar_url = %q, want GitHub avatar URL", byUser["alice"].AvatarURL)
	}
}

func TestHandlePresenceSnapshotLocalDoesNotLeakRoster(t *testing.T) {
	s := newTestServer()
	s.createUserSession("alice", "owner")

	rec := httptest.NewRecorder()
	s.handlePresenceSnapshot(rec, httptest.NewRequest("GET", "/api/presence", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /api/presence = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "alice") {
		t.Fatalf("local unauthenticated presence leaked session roster: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"mode":"local"`) || !strings.Contains(rec.Body.String(), `"username":"local"`) {
		t.Fatalf("local presence = %s, want local row", rec.Body.String())
	}
}

func TestPresenceAvatarSkipsOpaqueOrEmailIdentities(t *testing.T) {
	for _, user := range []string{"ibmid:5500", "alice@example.com", "bad/user", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "gho_tokenlike", "-alice", "alice-", "alice--corp"} {
		if isPlainGitHubUsername(user) {
			t.Fatalf("%q must not be treated as a GitHub username", user)
		}
	}
	if !isPlainGitHubUsername("clubanderson") {
		t.Fatal("plain GitHub username should be accepted")
	}
}

func TestHandlePresenceSnapshotRedactsEmailIdentity(t *testing.T) {
	s := newTestServer()
	s.createUserSession("alice@example.com", "owner")

	req := httptest.NewRequest("GET", "/api/presence", nil)
	req.Header.Set("X-Hive-User", "alice@example.com")
	rec := httptest.NewRecorder()
	s.handlePresenceSnapshot(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /api/presence = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "alice@example.com") || strings.Contains(body, "@") {
		t.Fatalf("presence leaked email identity: %s", body)
	}
	if !strings.Contains(body, `"username":"user-`) || strings.Contains(body, `"avatar_url"`) {
		t.Fatalf("presence body = %s, want redacted pseudonym without avatar", body)
	}
}

func TestHandlePresenceSnapshotRedactsEmailDisplayName(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}}
	s.deps.Config.Dashboard.AuthorizedUserNames = map[string]string{
		"ibmid:5500": "jane@example.com",
	}
	s.createUserSession("ibmid:5500", "owner")

	req := httptest.NewRequest("GET", "/api/presence", nil)
	req.Header.Set("X-Hive-User", "ibmid:5500")
	rec := httptest.NewRecorder()
	s.handlePresenceSnapshot(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /api/presence = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "jane@example.com") || strings.Contains(body, "ibmid:5500") || strings.Contains(body, "@") {
		t.Fatalf("presence leaked opaque/email display identity: %s", body)
	}
	if !strings.Contains(body, `"display_name":"user-`) {
		t.Fatalf("presence body = %s, want pseudonymous display fallback", body)
	}
}

// TestEngagedSessionUsernamesFreshness verifies that a user whose last engaged
// ping is older than presenceFreshness drops out (and is pruned), so a closed
// laptop stops reading as engaged within seconds, not heartbeats.
func TestEngagedSessionUsernamesFreshness(t *testing.T) {
	s := newTestServer()
	s.markUserEngaged("fresh", time.Now())
	s.markUserEngaged("stale", time.Now().Add(-2*presenceFreshness))

	got := s.EngagedSessionUsernames()
	if len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("engaged = %v, want [fresh] (stale pruned)", got)
	}
	s.presenceMu.Lock()
	_, still := s.presenceEngagedAt["stale"]
	s.presenceMu.Unlock()
	if still {
		t.Error("stale engaged entry must be pruned on read")
	}
}

// TestMarkUserEngagedBounded verifies the defensive cap: beyond
// maxPresenceUsers distinct usernames, NEW names are dropped while existing
// ones keep refreshing.
func TestMarkUserEngagedBounded(t *testing.T) {
	s := newTestServer()
	now := time.Now()
	for i := 0; i < maxPresenceUsers; i++ {
		s.markUserEngaged(fmt.Sprintf("user-%d", i), now)
	}
	s.markUserEngaged("overflow", now)
	s.presenceMu.Lock()
	_, added := s.presenceEngagedAt["overflow"]
	size := len(s.presenceEngagedAt)
	s.presenceMu.Unlock()
	if added || size != maxPresenceUsers {
		t.Errorf("cap breached: overflow added=%v size=%d (max %d)", added, size, maxPresenceUsers)
	}
	// Existing users still refresh at the cap.
	s.markUserEngaged("user-0", now.Add(time.Minute))
	s.presenceMu.Lock()
	refreshed := s.presenceEngagedAt["user-0"].Equal(now.Add(time.Minute))
	s.presenceMu.Unlock()
	if !refreshed {
		t.Error("an already-tracked user must keep refreshing at the cap")
	}
}

// TestAuditLastUserActions covers the cheap last-real-action signal: audit
// writes by real users update the per-user timestamp at write time, while
// pseudo-users (system/local/unknown — background jobs, unauthenticated
// access) never count as engagement.
func TestAuditLastUserActions(t *testing.T) {
	s := newTestServer()

	s.audit.Log("alice", "config_save", "", "")
	s.audit.Log("system", "startup", "", "")
	s.audit.Log("local", "config_save", "", "")
	s.audit.Log("unknown", "login_error", "", "")
	s.audit.Log("", "watcher", "", "") // Log maps "" to "system"

	// Read through the Server wrapper (the heartbeat's entry point). Membership
	// checks rather than an exact size: on a host with a real /data the audit
	// log may have replayed prior entries at construction.
	acts := s.UserLastActions()
	ts, ok := acts["alice"]
	if !ok {
		t.Fatalf("alice's audited action must be tracked, got %v", acts)
	}
	when, err := time.Parse(time.RFC3339, ts)
	if err != nil || time.Since(when) > time.Minute {
		t.Fatalf("alice's last action %q must be a fresh RFC3339 stamp (err=%v)", ts, err)
	}
	for _, pseudo := range []string{"system", "local", "unknown", ""} {
		if _, tracked := acts[pseudo]; tracked {
			t.Errorf("pseudo-user %q must never count as engagement", pseudo)
		}
	}
}

// TestNoteUserActionNeverRegresses verifies replaying an OLDER entry (rotated
// log files, out-of-order load) cannot move a user's last action backwards,
// and that an unparsable timestamp is ignored.
func TestNoteUserActionNeverRegresses(t *testing.T) {
	// A bare in-memory AuditLog (no disk attach) so the assertions are exact.
	a := &AuditLog{ring: make([]AuditEntry, 0, auditRingCap), lastAction: make(map[string]time.Time)}
	newer := time.Now().UTC().Format(time.RFC3339)
	older := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)

	a.mu.Lock()
	a.noteUserAction("alice", newer)
	a.noteUserAction("alice", older)
	a.noteUserAction("bob", "not-a-timestamp")
	a.mu.Unlock()

	acts := a.LastUserActions()
	if acts["alice"] != newer {
		t.Errorf("alice = %q, want the newer stamp %q", acts["alice"], newer)
	}
	if _, ok := acts["bob"]; ok {
		t.Error("an unparsable timestamp must not be tracked")
	}
}
