package askai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHub is a hub relay endpoint that records what a hive sends.
type fakeHub struct {
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
	status  int
	reply   string
	srv     *httptest.Server
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{status: http.StatusOK}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		h.mu.Lock()
		h.bodies = append(h.bodies, body)
		h.headers = append(h.headers, r.Header.Clone())
		status, reply := h.status, h.reply
		h.mu.Unlock()
		if r.URL.Path != RelayPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) set(status int, reply string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.reply = status, reply
}

func (h *fakeHub) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies)
}

func (h *fakeHub) last() (map[string]any, http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bodies[len(h.bodies)-1], h.headers[len(h.headers)-1]
}

const hubOKReply = `{"outcome":"ok","answer":"Open Settings, then Features. See [this](https://evil.example/x).",
	"sources":[{"title":"Settings","url":"https://docs.hivecommons.dev/settings/"},{"title":"Evil","url":"https://evil.example/"}],
	"conversation":"0123456789abcdef0123456789abcdef","ai_generated":false,"docs_url":"https://evil.example/docs"}`

type consentSet map[string]bool

func (c consentSet) Has(p string) bool { return c[p] }

func newTestRelay(t *testing.T, hub *fakeHub, consent ConsentChecker) *RelayAsker {
	t.Helper()
	a, err := NewRelayAsker(RelayOptions{
		HubURL: hub.srv.URL + "/", HiveID: "hosted-hive-1", Bearer: func() string { return "per-hive-bearer" },
		Consent: consent, Limits: generous, HTTP: hub.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestRelaySendsOnlyTheQuestionAndRefiltersTheAnswer: the hive forwards
// question, section and the hub's own handle, authenticated as the hive, and
// never trusts the hub's links, label or docs URL.
func TestRelaySendsOnlyTheQuestionAndRefiltersTheAnswer(t *testing.T) {
	hub := newFakeHub(t)
	hub.set(http.StatusOK, hubOKReply)
	a := newTestRelay(t, hub, consentSet{"alice-login": true, "bob": true})

	r := a.Ask(context.Background(), Request{Person: "alice-login", HiveID: "ignored", Question: " Where is pause? ", Section: "agents-section"})
	if r.Outcome != OutcomeOK || len(r.Sources) != 1 || r.Sources[0].URL != "https://docs.hivecommons.dev/settings/" ||
		strings.Contains(r.Answer, "evil.example") || !r.AIGenerated || r.DocsURL != DocsHomeURL {
		t.Fatalf("result %+v", r)
	}
	body, hdr := hub.last()
	if len(body) != 2 || body["question"] != "Where is pause?" || body["section"] != "agents-section" {
		t.Fatalf("relay body must be the question and section only, got %v", body)
	}
	if hdr.Get("Authorization") != "Bearer per-hive-bearer" || hdr.Get(RelayHiveIDHeader) != "hosted-hive-1" {
		t.Fatalf("relay headers %v", hdr)
	}
	if r.Conversation == "" || r.Conversation == "0123456789abcdef0123456789abcdef" {
		t.Fatalf("the hub's handle must stay on the hive behind a local one, got %q", r.Conversation)
	}

	// Follow-up: alice's local handle maps to the hub handle; bob's use of it
	// sends no conversation at all.
	a.Ask(context.Background(), Request{Person: "alice-login", Question: "and then?", Conversation: r.Conversation})
	if body, _ := hub.last(); body["conversation"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("follow-up body %v", body)
	}
	a.Ask(context.Background(), Request{Person: "bob", Question: "and then?", Conversation: r.Conversation, Section: "acme/private-repo"})
	if body, _ := hub.last(); body["conversation"] != nil || body["section"] != nil {
		t.Fatalf("bob used alice's handle or an unknown section was sent: %v", body)
	}
}

func TestRelayNothingLeavesWithoutConsentOrWithACredential(t *testing.T) {
	hub := newFakeHub(t)
	hub.set(http.StatusOK, hubOKReply)
	a := newTestRelay(t, hub, consentSet{"alice": true})
	if r := a.Ask(context.Background(), Request{Person: "mallory", Question: "q?"}); r.Outcome != OutcomeNotConsented {
		t.Fatalf("no consent: %+v", r)
	}
	if r := a.Ask(context.Background(), Request{Person: "alice", Question: "ghp_abcdefghijklmnopqrstuvwxyz0123 fails"}); r.Outcome != OutcomeRefusedSecretShape {
		t.Fatalf("credential: %+v", r)
	}
	if r := a.Ask(context.Background(), Request{Person: "alice", Question: strings.Repeat("x", MaxQuestionRunes+1)}); r.Outcome != OutcomeInvalidQuestion {
		t.Fatalf("too long: %+v", r)
	}
	if hub.calls() != 0 {
		t.Fatalf("the hub was called %d times", hub.calls())
	}
	nilConsent := newTestRelay(t, hub, nil)
	if r := nilConsent.Ask(context.Background(), Request{Person: "alice", Question: "q?"}); r.Outcome != OutcomeNotConsented || hub.calls() != 0 {
		t.Fatalf("nil checker: %+v", r)
	}
}

func TestRelayFailuresFallBackWithoutAKey(t *testing.T) {
	hub := newFakeHub(t)
	a := newTestRelay(t, hub, consentSet{"alice": true})
	ask := func() Outcome { return a.Ask(context.Background(), Request{Person: "alice", Question: "q?"}).Outcome }
	for status, want := range map[int]Outcome{
		http.StatusNotFound:            OutcomeRelayDisabled,
		http.StatusUnauthorized:        OutcomeRelayDisabled,
		http.StatusForbidden:           OutcomeRelayDisabled,
		http.StatusBadGateway:          OutcomeUpstreamError,
		http.StatusInternalServerError: OutcomeUpstreamError,
	} {
		hub.set(status, `{}`)
		if got := ask(); got != want {
			t.Errorf("hub %d: %s, want %s", status, got, want)
		}
	}
	hub.set(http.StatusOK, `{"outcome":"something_new"}`)
	if got := ask(); got != OutcomeUpstreamError {
		t.Fatalf("unknown outcome: %s", got)
	}
	hub.set(http.StatusOK, `{"outcome":"ok","answer":"text","sources":[{"url":"https://evil.example/"}]}`)
	if got := ask(); got != OutcomeNoCitedAnswer {
		t.Fatalf("uncited relayed answer: %s", got)
	}
	hub.set(http.StatusOK, `{"outcome":"rate_limited","message":"Busy, try again in 9 seconds.","retry_after_seconds":9}`)
	if r := a.Ask(context.Background(), Request{Person: "alice", Question: "q?"}); r.Outcome != OutcomeRateLimited || r.RetryAfterSeconds != 9 {
		t.Fatalf("relayed busy: %+v", r)
	}
	if _, err := NewRelayAsker(RelayOptions{HiveID: "h", Bearer: func() string { return "b" }}); err == nil {
		t.Fatal("a relay without a hub URL must be refused")
	}
}

func TestRelayAppliesThisHivesPersonLimits(t *testing.T) {
	hub := newFakeHub(t)
	hub.set(http.StatusOK, hubOKReply)
	a, err := NewRelayAsker(RelayOptions{
		HubURL: hub.srv.URL, HiveID: "h", Bearer: func() string { return "b" }, Consent: consentSet{"alice": true},
		Limits: Limits{PersonPerMinute: 1, PersonPerHour: 10, GlobalPerMinute: 10, InFlight: 10, Daily: 10},
		Now:    func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Ask(context.Background(), Request{Person: "alice", Question: "q?"})
	if r := a.Ask(context.Background(), Request{Person: "alice", Question: "q?"}); r.Outcome != OutcomeRateLimited || hub.calls() != 1 {
		t.Fatalf("per-person limit on the hive: %+v, calls %d", r, hub.calls())
	}
}

// TestServiceRelayChargesTheHiveNotAPerson is the hub side: no consent check
// (the hive took it), the hive's hourly cap applies, and a conversation handle
// is bound to the hive that created it.
func TestServiceRelayChargesTheHiveNotAPerson(t *testing.T) {
	up := &fakeUpstream{answer: citedAnswer}
	limits := Limits{PersonPerMinute: 1, PersonPerHour: 1, HivePerHour: 2, GlobalPerMinute: 10, InFlight: 10, Daily: 10}
	svc := newTestService(t, up, nil, limits, nil, nil)
	first := svc.Relay(context.Background(), Request{HiveID: "hive-a", Question: "q?"})
	if first.Outcome != OutcomeOK || first.Conversation == "" {
		t.Fatalf("relay without a consent checker: %+v", first)
	}
	// Per-person caps (1) do not bind relayed traffic; the hive cap (2) does.
	if r := svc.Relay(context.Background(), Request{HiveID: "hive-a", Question: "q?", Conversation: first.Conversation}); r.Outcome != OutcomeOK {
		t.Fatalf("second relay: %+v", r)
	}
	if r := svc.Relay(context.Background(), Request{HiveID: "hive-a", Question: "q?"}); r.Outcome != OutcomeRateLimited {
		t.Fatalf("hive cap: %+v", r)
	}
	if r := svc.Relay(context.Background(), Request{HiveID: "hive-b", Question: "q?", Conversation: first.Conversation}); r.Outcome != OutcomeOK {
		t.Fatalf("hive B: %+v", r)
	}
	want := []string{"", citedAnswer.ThreadID, ""}
	for i, got := range up.threads {
		if got != want[i] {
			t.Fatalf("call %d thread %q, want %q (hive B must not continue hive A's thread)", i, got, want[i])
		}
	}
	if r := svc.Relay(context.Background(), Request{Question: "q?"}); r.Outcome != OutcomeRelayDisabled {
		t.Fatalf("no hive: %+v", r)
	}
	if svc.RelayDisabledResult().Outcome != OutcomeRelayDisabled {
		t.Fatal("disabled result")
	}
}

func TestPersonKey(t *testing.T) {
	if PersonKey("alice") != "alice" || PersonKey("  ") != "" {
		t.Fatal("a safe login is its own key")
	}
	k := PersonKey("alice@example.com")
	if !strings.HasPrefix(k, personKeyHashPrefix) || strings.Contains(k, "@") || k != PersonKey("alice@example.com") || !consentStemPattern.MatchString(k) {
		t.Fatalf("hashed key %q", k)
	}
	if PersonKey("../etc/passwd") == "../etc/passwd" {
		t.Fatal("a path must never be a key")
	}
}
