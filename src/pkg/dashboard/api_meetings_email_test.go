package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/mailallow"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

const testMailRelayKey = "relay-key-mail-api"

type fakeInviteScheduler struct {
	fakeScheduleService
	got     []wire.Delivery
	trusted []bool
	err     error
}

func (f *fakeInviteScheduler) IngestInvites(_ context.Context, d wire.Delivery, trusted bool) (any, error) {
	f.got, f.trusted = append(f.got, d), append(f.trusted, trusted)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"results": []any{}}, nil
}

// mailServer is an API server with a relay key, an allowlist store and a
// live access list of alice (owner) and bob (read).
func mailServer(t *testing.T) (*Server, *mailallow.Store) {
	t.Helper()
	withWebhookRelayKey(t, testMailRelayKey)
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUsers = []string{"alice", "bob:read"}
	deps.Config.Hub.DashboardURL = "https://acme.hive.hivecommons.dev/"
	st, err := mailallow.Open(filepath.Join(t.TempDir(), "allowlist.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetMailAllowlist(st)
	return s, st
}

var relaySeq int

// relayPost posts a delivery as the hub would: signed with the relay key.
func relayPost(s *Server, path string, d wire.Delivery, key string) *httptest.ResponseRecorder {
	relaySeq++
	if d.ID == "" {
		d.ID = "deliv" + strconv.Itoa(relaySeq)
	}
	body, _ := json.Marshal(d)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(webhookRelayTimestampHeader, ts)
	req.Header.Set(webhookRelaySignatureHeader, signWebhookRelayForTest(key, ts, d.ID, body))
	req.Header.Set(webhookRelayDeliveryHeader, d.ID)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func userReq(method, path, user, role, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if role == "owner" {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	return req
}

func serve(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestMailEndpointsArePublicButSigned(t *testing.T) {
	for _, p := range []string{meetingsInvitesPath, meetingsCalendarProofPath, inboxEmailPath} {
		if !isPublicPath(p) {
			t.Errorf("%s must be reachable without a session", p)
		}
	}
	if isPublicPath(meetingsCalendarEmailsPath) {
		t.Error("the calendar-emails API needs a session")
	}
	s, _ := mailServer(t)
	d := wire.Delivery{Kind: wire.KindInvite}
	if rec := relayPost(s, meetingsInvitesPath, d, "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	// Signed for one delivery id, presented as another.
	body, _ := json.Marshal(wire.Delivery{ID: "a", Kind: wire.KindInvite})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, meetingsInvitesPath, bytes.NewReader(body))
	req.Header.Set(webhookRelayTimestampHeader, ts)
	req.Header.Set(webhookRelaySignatureHeader, signWebhookRelayForTest(testMailRelayKey, ts, "b", body))
	req.Header.Set(webhookRelayDeliveryHeader, "b")
	if rec := serve(s, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("delivery id mismatch: %d", rec.Code)
	}
	// Wrong kind for the endpoint.
	if rec := relayPost(s, meetingsInvitesPath, wire.Delivery{Kind: wire.KindProof}, testMailRelayKey); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong kind: %d", rec.Code)
	}
	// A replayed delivery.
	d.ID = "replay-1"
	_ = relayPost(s, meetingsInvitesPath, d, testMailRelayKey)
	if rec := relayPost(s, meetingsInvitesPath, d, testMailRelayKey); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay: %d", rec.Code)
	}
	// Too large.
	big := httptest.NewRequest(http.MethodPost, meetingsInvitesPath, bytes.NewReader(make([]byte, mailRelayMaxBody+1)))
	if rec := serve(s, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", rec.Code)
	}
}

func TestMailRelayWithoutKey(t *testing.T) {
	withWebhookRelayKey(t, "")
	s, _ := apiServer(t)
	if rec := relayPost(s, meetingsInvitesPath, wire.Delivery{Kind: wire.KindInvite}, "k"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no relay key: %d", rec.Code)
	}
}

func proveFor(t *testing.T, st *mailallow.Store, login, email string) {
	t.Helper()
	e, err := st.Add(login, email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Prove(e.Code, email, true); err != nil {
		t.Fatal(err)
	}
}

func TestMeetingsInvitesTrustDecision(t *testing.T) {
	s, st := mailServer(t)
	proveFor(t, st, "bob", "bob@example.com")
	proveFor(t, st, "carol", "carol@example.com") // carol has no access

	inv := func(org string) wire.Invite { return wire.Invite{UID: "u", Organizer: &wire.Person{Email: org}} }
	if rec := relayPost(s, meetingsInvitesPath, wire.Delivery{Kind: wire.KindInvite, Invites: []wire.Invite{inv("bob@example.com")}}, testMailRelayKey); rec.Code != http.StatusConflict {
		t.Fatalf("no scheduler: %d", rec.Code)
	}
	f := &fakeInviteScheduler{}
	s.SetMeetingsScheduleService(f)
	cases := []struct {
		from    string
		aligned bool
		org     string
		want    bool
	}{
		{"bob@example.com", true, "BOB@example.com", true},
		{"bob@example.com", false, "bob@example.com", false},    // not aligned
		{"bob@example.com", true, "boss@example.com", false},    // sender is not the organizer
		{"mallory@x.org", true, "bob@example.com", false},       // organizer allowlisted, sender not
		{"carol@example.com", true, "carol@example.com", false}, // access revoked
	}
	for i, c := range cases {
		d := wire.Delivery{Kind: wire.KindInvite, Auth: wire.Auth{From: c.from, Aligned: c.aligned}, Invites: []wire.Invite{inv(c.org)}}
		if rec := relayPost(s, meetingsInvitesPath, d, testMailRelayKey); rec.Code != http.StatusOK {
			t.Fatalf("case %d: %d %s", i, rec.Code, rec.Body)
		}
		if got := f.trusted[len(f.trusted)-1]; got != c.want {
			t.Errorf("case %d: trusted %v, want %v", i, got, c.want)
		}
	}
	f.err = errors.Join(ErrMeetingInvalid, errors.New("bad"))
	if rec := relayPost(s, meetingsInvitesPath, wire.Delivery{Kind: wire.KindInvite}, testMailRelayKey); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid invites: %d", rec.Code)
	}
	f.err = errors.New("disk full")
	if rec := relayPost(s, meetingsInvitesPath, wire.Delivery{Kind: wire.KindInvite}, testMailRelayKey); rec.Code < 500 {
		t.Fatalf("scheduler failure: %d", rec.Code)
	}
	// A scheduler that cannot take invites.
	s.SetMeetingsScheduleService(&fakeScheduleService{})
	if rec := relayPost(s, meetingsInvitesPath, wire.Delivery{Kind: wire.KindInvite}, testMailRelayKey); rec.Code != http.StatusConflict {
		t.Fatalf("no ingester: %d", rec.Code)
	}
	// Invites switched off.
	t.Setenv("HIVE_MEETINGS_EMAIL_INVITES_ENABLED", "off")
	s.SetMeetingsScheduleService(f)
	if rec := relayPost(s, meetingsInvitesPath, wire.Delivery{Kind: wire.KindInvite}, testMailRelayKey); rec.Code != http.StatusConflict {
		t.Fatalf("invites off: %d", rec.Code)
	}
}

func TestCalendarEmailProof(t *testing.T) {
	s, st := mailServer(t)
	e, _ := st.Add("bob", "bob@example.com")
	proof := func(code, from string, aligned bool) *httptest.ResponseRecorder {
		return relayPost(s, meetingsCalendarProofPath, wire.Delivery{Kind: wire.KindProof, Auth: wire.Auth{From: from, Aligned: aligned}, Proof: &wire.Proof{Code: code}}, testMailRelayKey)
	}
	if rec := proof(e.Code, "bob@example.com", false); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unaligned proof: %d", rec.Code)
	}
	if rec := proof("hive-proof-aaaaaaaaaaaa", "bob@example.com", true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown code: %d", rec.Code)
	}
	if rec := proof(e.Code, "bob@example.com", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"proven":true`) {
		t.Fatalf("proof: %d %s", rec.Code, rec.Body)
	}
	if rec := relayPost(s, meetingsCalendarProofPath, wire.Delivery{Kind: wire.KindProof}, testMailRelayKey); rec.Code != http.StatusConflict {
		t.Fatalf("no proof block: %d", rec.Code)
	}
	s.SetMailAllowlist(nil)
	if rec := proof(e.Code, "bob@example.com", true); rec.Code != http.StatusConflict {
		t.Fatalf("no store: %d", rec.Code)
	}
}

func TestCalendarEmailsAPI(t *testing.T) {
	s, _ := mailServer(t)
	path := meetingsCalendarEmailsPath
	if rec := serve(s, userReq(http.MethodGet, path, "bob", "", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("no role: %d", rec.Code)
	}
	unverified := userReq(http.MethodGet, path, "alice", "owner", "")
	unverified.Header.Del(ownerRoleVerifiedHeader)
	if rec := serve(s, unverified); rec.Code != http.StatusForbidden {
		t.Fatalf("unverified owner: %d", rec.Code)
	}

	rec := serve(s, userReq(http.MethodPost, path, "bob", "read", `{"email":"Bob@Example.com"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	var added struct {
		Email       calendarEmailView `json:"email"`
		Address     string            `json:"address"`
		Instruction string            `json:"instruction"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &added)
	if added.Address != "hive@acme.hive.hivecommons.dev" || added.Email.Code == "" || !strings.Contains(added.Instruction, added.Email.Code) || !strings.Contains(added.Instruction, added.Address) || !added.Email.Active {
		t.Fatalf("added %+v", added)
	}
	for body, want := range map[string]int{`{"email":"nope"}`: http.StatusBadRequest, `{`: http.StatusBadRequest} {
		if rec := serve(s, userReq(http.MethodPost, path, "bob", "read", body)); rec.Code != want {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}

	rec = serve(s, userReq(http.MethodGet, path, "bob", "read", ""))
	var list struct {
		Address   string              `json:"address"`
		Emails    []calendarEmailView `json:"emails"`
		Allowlist []calendarEmailView `json:"allowlist"`
		Invites   bool                `json:"invites_enabled"`
		Files     bool                `json:"files_enabled"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Emails) != 1 || list.Emails[0].Email != "bob@example.com" || list.Allowlist != nil || !list.Invites || !list.Files {
		t.Fatalf("bob's list %+v", list)
	}
	// Only the owner sees the whole allowlist, without codes.
	if rec := serve(s, userReq(http.MethodGet, path+"?all=1", "bob", "read", "")); strings.Contains(rec.Body.String(), `"allowlist"`) {
		t.Fatal("a reader saw the allowlist")
	}
	rec = serve(s, userReq(http.MethodGet, path+"?all=1", "alice", "owner", ""))
	list.Allowlist = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Allowlist) != 1 || list.Allowlist[0].Login != "bob" || list.Allowlist[0].Code != "" {
		t.Fatalf("owner allowlist %+v", list.Allowlist)
	}

	if rec := serve(s, userReq(http.MethodDelete, path+"?email=bob@example.com", "alice", "read", "")); rec.Code != http.StatusNotFound {
		t.Fatalf("someone else's address: %d", rec.Code)
	}
	if rec := serve(s, userReq(http.MethodDelete, path+"?email=bob@example.com&login=bob", "alice", "owner", "")); rec.Code != http.StatusOK {
		t.Fatalf("owner removes bob's address: %d", rec.Code)
	}
	if rec := serve(s, userReq(http.MethodDelete, path+"?email=bob@example.com", "bob", "read", "")); rec.Code != http.StatusNotFound {
		t.Fatalf("already removed: %d", rec.Code)
	}

	s.SetMailAllowlist(nil)
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if rec := serve(s, userReq(m, path, "bob", "read", `{"email":"bob@example.com"}`)); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without store: %d", m, rec.Code)
		}
	}
}

func TestMailAddressFallsBackToHost(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.DashboardURL = ""
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "Beta.Example:8443"
	if got := s.mailAddress(req); got != "hive@beta.example" {
		t.Fatalf("address %q", got)
	}
	s.deps = nil
	req.Host = "plain.example"
	if got := s.mailAddress(req); got != "hive@plain.example" {
		t.Fatalf("address %q", got)
	}
	if s.hasMailAccess("anyone") || !s.meetingsEmailConfig().InvitesOn() {
		t.Fatal("no config: no access, invites default on")
	}
}

func TestCalendarEmailsAddLimits(t *testing.T) {
	s, _ := mailServer(t)
	for i := 0; i < mailallow.MaxPerUser; i++ {
		body := `{"email":"bob` + strconv.Itoa(i) + `@example.com"}`
		if rec := serve(s, userReq(http.MethodPost, meetingsCalendarEmailsPath, "bob", "read", body)); rec.Code != http.StatusCreated {
			t.Fatalf("add %d: %d", i, rec.Code)
		}
	}
	if rec := serve(s, userReq(http.MethodPost, meetingsCalendarEmailsPath, "bob", "read", `{"email":"more@example.com"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("over the cap: %d", rec.Code)
	}
}

func TestMailRelayPathsMatchWire(t *testing.T) {
	if meetingsInvitesPath != wire.SpokeInvitesPath || meetingsCalendarProofPath != wire.SpokeProofPath || inboxEmailPath != wire.SpokeFilesPath || inboxEmailChunkPath != wire.SpokeFilesChunkPath || inboxEmailReportPath != wire.SpokeReportPath {
		t.Fatal("the hub forwards to wire.Spoke*Path; the dashboard must serve the same paths")
	}
	// Every relay target must pass the session gate: the hub holds no
	// dashboard session, and each handler verifies the relay signature
	// itself. The chunk path was missing (hivecommons/hive#18699), so every
	// mail with more than one relay piece of attachments got a 401.
	for _, p := range []string{meetingsInvitesPath, meetingsCalendarProofPath, inboxEmailPath, inboxEmailChunkPath, inboxEmailReportPath} {
		if !isPublicPath(p) {
			t.Errorf("isPublicPath(%q) = false: the hub's relay cannot reach it", p)
		}
	}
	for _, p := range []string{"/api/inbox/email/recent", "/api/inbox/email/abc/file", meetingsCalendarEmailsPath} {
		if isPublicPath(p) {
			t.Errorf("isPublicPath(%q) = true: it must stay behind the session gate", p)
		}
	}
}

// TestCalendarEmailProofFailureReasons (hivecommons/hive#18579): each
// refused proof shows its reason, time and authentication results on the
// user's row in plain words; an unknown code shows, masked, only in the
// owner's recent proof attempts.
func TestCalendarEmailProofFailureReasons(t *testing.T) {
	s, st := mailServer(t)
	e, _ := st.Add("bob", "bob@example.com")
	proof := func(code string, auth wire.Auth) {
		t.Helper()
		d := wire.Delivery{Kind: wire.KindProof, Auth: auth, Proof: &wire.Proof{Code: code}}
		if rec := relayPost(s, meetingsCalendarProofPath, d, testMailRelayKey); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("proof %+v: %d %s", auth, rec.Code, rec.Body)
		}
	}
	type listing struct {
		Emails   []calendarEmailView      `json:"emails"`
		Attempts []mailallow.ProofAttempt `json:"recent_proof_attempts"`
	}
	list := func(user, role string) (listing, string) {
		t.Helper()
		rec := serve(s, userReq(http.MethodGet, meetingsCalendarEmailsPath, user, role, ""))
		var l listing
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		return l, rec.Body.String()
	}
	cases := []struct {
		auth   wire.Auth
		reason string
		want   string
	}{
		{wire.Auth{From: "bob@example.com", FromDomain: "example.com", SPF: "fail", SPFDomain: "example.com", DKIM: []wire.DKIMResult{{Domain: "example.com", Result: "fail"}}, DMARC: "fail"},
			mailallow.ReasonCodeNotAligned, "neither SPF nor DKIM passed for example.com. Turn on DKIM"},
		{wire.Auth{From: "eve@other.example", FromDomain: "other.example", SPF: "pass", Aligned: true},
			mailallow.ReasonCodeFromMismatch, "different address than the one registered: the mail's From was eve@other.example. Send it from bob@example.com"},
	}
	for _, c := range cases {
		proof(e.Code, c.auth)
		l, body := list("bob", "read")
		f := l.Emails[0].LastFailure
		if f == nil || f.Reason != c.reason || f.At.IsZero() || !strings.Contains(f.Message, c.want) || f.SPF != c.auth.SPF || f.Aligned != c.auth.Aligned {
			t.Fatalf("%s: %+v", c.reason, f)
		}
		if strings.Contains(body, "recent_proof_attempts") {
			t.Fatal("a reader saw the proof attempts")
		}
	}
	// The JSON field names the UI reads.
	_, body := list("bob", "read")
	for _, k := range []string{`"last_failure":{`, `"reason":"from_mismatch"`, `"from":"eve@other.example"`, `"spf":"pass"`, `"aligned":true`, `"message":"`} {
		if !strings.Contains(body, k) {
			t.Errorf("list misses %s: %s", k, body)
		}
	}

	// Unknown code: owner-only, masked.
	proof("hive-proof-zzzzzzzzzzzz", wire.Auth{From: "Typo@Example.org", Aligned: true})
	l, _ := list("alice", "owner")
	if len(l.Attempts) != 1 || l.Attempts[0].From != "t***@example.org" || l.Attempts[0].Reason != mailallow.AttemptCodeNotRecognized {
		t.Fatalf("owner attempts %+v", l.Attempts)
	}
	if _, body := list("bob", "read"); strings.Contains(body, "t***@example.org") {
		t.Fatal("a reader saw an unknown-code attempt")
	}

	// Expired code: still recognized, the row says it expired.
	now := time.Now()
	clockStore, err := mailallow.Open(filepath.Join(t.TempDir(), "a.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	old, _ := clockStore.Add("bob", "old@example.com")
	now = now.Add(mailallow.CodeTTL + time.Hour)
	s.SetMailAllowlist(clockStore)
	proof(old.Code, wire.Auth{From: "old@example.com", Aligned: true})
	if l, _ := list("bob", "read"); l.Emails[0].LastFailure == nil || l.Emails[0].LastFailure.Reason != mailallow.ReasonCodeExpired || !strings.Contains(l.Emails[0].LastFailure.Message, "The code expired") {
		t.Fatalf("expired %+v", l.Emails)
	}
}

func TestProofFailureMessage(t *testing.T) {
	got := proofFailureMessage(mailallow.ProofFailure{Reason: mailallow.ReasonCodeNotAligned}, "ann@clubs.example")
	if !strings.Contains(got, "passed for clubs.example.") {
		t.Fatalf("not aligned without From domain: %q", got)
	}
	if got := proofFailureMessage(mailallow.ProofFailure{Reason: mailallow.ReasonCodeFromMismatch}, "a@b.example"); !strings.Contains(got, "From was "+proofFromNone+".") {
		t.Fatalf("from mismatch without From: %q", got)
	}
	if got := proofFailureMessage(mailallow.ProofFailure{Reason: "other"}, "a@b.example"); !strings.Contains(got, "did not count") {
		t.Fatalf("unknown reason: %q", got)
	}
}
