package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func resetCustomerMatchStore(t *testing.T) {
	t.Helper()
	reset := func() {
		customerMatchStore.mu.Lock()
		customerMatchStore.indexes, customerMatchStore.rates = nil, nil
		customerMatchStore.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func resetCustomerMatchRates() {
	customerMatchStore.mu.Lock()
	customerMatchStore.rates = nil
	customerMatchStore.mu.Unlock()
}

type matchHarness struct {
	t    *testing.T
	s    *HubServer
	logs *bytes.Buffer
}

func newMatchHarness(t *testing.T) *matchHarness {
	t.Helper()
	resetCustomerMatchStore(t)
	s := newHandlerHub()
	s.mux = http.NewServeMux()
	s.registerCustomerSharingRoutes()
	logs := new(bytes.Buffer)
	s.logger = slog.New(slog.NewTextHandler(logs, nil))
	return &matchHarness{t: t, s: s, logs: logs}
}

func (h *matchHarness) post(bearer, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodPost, customerMatchPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.s.mux.ServeHTTP(w, r)
	return w
}

func (h *matchHarness) match(asHive, claimHive string, hashes ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	b, err := json.Marshal(customerMatchRequest{HiveID: claimHive, Hashes: hashes})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.post(h.s.heartbeatKeyFor(asHive), string(b))
}

func (h *matchHarness) answer(w *httptest.ResponseRecorder) customerMatchResponse {
	h.t.Helper()
	if w.Code != http.StatusOK {
		h.t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp customerMatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func matchHash(hive, login string) string {
	return customerMatchHash(customerMatchKey(testHubSecret, hive), login)
}

func saveMatchUser(t *testing.T, u *SaaSUser) *SaaSUser {
	t.Helper()
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	return u
}

func writeMatchConsent(t *testing.T, u *SaaSUser, hive string, edit func(*customerConsent)) {
	t.Helper()
	path, err := customerConsentPath(u)
	if err != nil {
		t.Fatal(err)
	}
	records, err := readCustomerConsents(path)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := records[hive]
	if !ok {
		now := time.Now().UTC()
		c = customerConsent{Person: u.GitHubUsername, HiveID: hive, TextVersion: customerConsentTextVersion, GrantedAt: now, UpdatedAt: now}
	}
	edit(&c)
	records[hive] = c
	if err := saveCustomerConsents(path, records); err != nil {
		t.Fatal(err)
	}
}

func readMatchConsent(t *testing.T, u *SaaSUser, hive string) customerConsent {
	t.Helper()
	path, err := customerConsentPath(u)
	if err != nil {
		t.Fatal(err)
	}
	records, err := readCustomerConsents(path)
	if err != nil {
		t.Fatal(err)
	}
	return records[hive]
}

func TestCustomerMatchAnswersOnlyForConsent(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "")
	h := newMatchHarness(t)
	one := map[string]string{"one": "viewer"}
	alice := saveMatchUser(t, &SaaSUser{
		GitHubUsername: "alice", Hives: map[string]string{"one": "viewer", "two": "viewer"},
		FullName: "Alice Real", FullNameSource: countrySourceUser,
		Company: "Admin Co", CompanySource: countrySourceAdmin,
		Country: "GB", CountrySource: countrySourceInferred,
	})
	saveMatchUser(t, &SaaSUser{GitHubUsername: "bob", Hives: one, FullName: "Bob Real", FullNameSource: countrySourceUser})
	carol := saveMatchUser(t, &SaaSUser{GitHubUsername: "carol", Hives: one, FullName: "Carol Real", FullNameSource: countrySourceUser})
	dave := saveMatchUser(t, &SaaSUser{GitHubUsername: "dave", Hives: one, FullName: "Dave Real", FullNameSource: countrySourceUser})
	erin := saveMatchUser(t, &SaaSUser{GitHubUsername: "erin", Hives: one, FullName: "Erin Real", FullNameSource: countrySourceUser})
	frank := saveMatchUser(t, &SaaSUser{GitHubUsername: "frank", Hives: one, FullName: "Frank Real", FullNameSource: countrySourceUser, Blocked: true})
	gina := saveMatchUser(t, &SaaSUser{GitHubUsername: "gina", Hives: map[string]string{}, FullName: "Gina Real", FullNameSource: countrySourceUser})
	all := customerConsentFields{RealName: true, Organization: true, Country: true}
	name := customerConsentFields{RealName: true}
	writeMatchConsent(t, alice, "one", func(c *customerConsent) { c.Fields = all })
	writeMatchConsent(t, carol, "one", func(c *customerConsent) { c.Fields = name })
	withdrawn := time.Now().UTC()
	writeMatchConsent(t, dave, "one", func(c *customerConsent) { c.WithdrawnAt = &withdrawn })
	writeMatchConsent(t, erin, "one", func(c *customerConsent) { c.Fields, c.TextVersion = name, "0" })
	writeMatchConsent(t, frank, "one", func(c *customerConsent) { c.Fields = name })
	writeMatchConsent(t, gina, "one", func(c *customerConsent) { c.Fields = name }) // grant lost

	// Absent while the hub switch is off, even with a valid bearer.
	if w := h.match("one", "one", matchHash("one", "alice")); w.Code != http.StatusNotFound {
		t.Fatalf("switch off: status %d, want 404", w.Code)
	}
	t.Setenv(customersShareEnv, "true")

	// The bearer is bound to the claimed hive.
	if w := h.post("", `{"hive_id":"one","hashes":[]}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: status %d", w.Code)
	}
	if w := h.match("two", "one", matchHash("one", "alice")); w.Code != http.StatusUnauthorized {
		t.Fatalf("hive two bearer claiming hive one: status %d", w.Code)
	}

	// Unknown, unconsented, withdrawn-before-delivery, stale text, blocked and
	// lost grant are all the same silence.
	unknown := h.match("one", "one", matchHash("one", "nobody-here")).Body.String()
	if unknown != "{\"matches\":[]}\n" {
		t.Fatalf("unknown answer = %q", unknown)
	}
	for _, login := range []string{"bob", "dave", "erin", "frank", "gina"} {
		resetCustomerMatchRates()
		if got := h.match("one", "one", matchHash("one", login)).Body.String(); got != unknown {
			t.Fatalf("%s answer %q differs from unknown %q", login, got, unknown)
		}
	}

	// Admin-sourced and inferred values never cross, even with consent.
	resetCustomerMatchRates()
	w := h.match("one", "one", matchHash("one", "alice"), matchHash("one", "carol"), matchHash("one", "bob"))
	if body := w.Body.String(); strings.Contains(body, "Admin Co") || strings.Contains(body, "GB") {
		t.Fatalf("admin or inferred value crossed: %s", body)
	}
	resp := h.answer(w)
	if len(resp.Matches) != 2 {
		t.Fatalf("want alice and carol, got %+v", resp.Matches)
	}
	a, c := resp.Matches[0], resp.Matches[1]
	if a.MatchHash != matchHash("one", "alice") || a.RealName != "Alice Real" || a.Organization != "" ||
		a.Country != "" || a.Rev == "" || len(a.Cleared) != 0 {
		t.Fatalf("alice entry = %+v", a)
	}
	if c.MatchHash != matchHash("one", "carol") || c.RealName != "Carol Real" {
		t.Fatalf("carol entry = %+v", c)
	}

	// Delivery stamps on the record.
	ac := readMatchConsent(t, alice, "one")
	if ac.FirstDeliveredAt == nil || ac.LastDeliveredAt == nil || ac.DeliveredFields == nil ||
		*ac.DeliveredFields != name || ac.DeliveredRev != a.Rev ||
		ac.LastDeliveredFields == nil || *ac.LastDeliveredFields != name {
		t.Fatalf("alice delivery stamp = %+v", ac)
	}
	firstDelivered := *ac.FirstDeliveredAt

	// A user-entered country now crosses too, and widens the delivered set.
	alice.Country, alice.CountrySource = "FR", countrySourceUser
	saveMatchUser(t, alice)
	resetCustomerMatchRates()
	resp = h.answer(h.match("one", "one", matchHash("one", "alice")))
	if len(resp.Matches) != 1 || resp.Matches[0].Country != "FR" || resp.Matches[0].Rev == a.Rev {
		t.Fatalf("user country not delivered with a new rev: %+v", resp.Matches)
	}
	if ac = readMatchConsent(t, alice, "one"); !ac.FirstDeliveredAt.Equal(firstDelivered) ||
		*ac.DeliveredFields != (customerConsentFields{RealName: true, Country: true}) ||
		*ac.LastDeliveredFields != (customerConsentFields{RealName: true, Country: true}) {
		t.Fatalf("restamp = %+v", ac)
	}

	// cleared only follows a delivery: carol withdraws, alice's name loses
	// user provenance; dave (withdrew before any delivery) stays silent.
	writeMatchConsent(t, carol, "one", func(c *customerConsent) {
		now := time.Now().UTC()
		c.Fields, c.WithdrawnAt = customerConsentFields{}, &now
	})
	alice.FullNameSource = countrySourceAdmin
	saveMatchUser(t, alice)
	resetCustomerMatchRates()
	resp = h.answer(h.match("one", "one", matchHash("one", "alice"), matchHash("one", "carol"), matchHash("one", "dave")))
	if len(resp.Matches) != 2 {
		t.Fatalf("want alice and carol entries, got %+v", resp.Matches)
	}
	a, c = resp.Matches[0], resp.Matches[1]
	if a.RealName != "" || a.Country != "FR" || strings.Join(a.Cleared, ",") != "real_name" {
		t.Fatalf("alice after provenance loss = %+v", a)
	}
	if c.RealName != "" || c.Rev != "" || strings.Join(c.Cleared, ",") != "real_name" {
		t.Fatalf("carol after withdrawal = %+v", c)
	}
	if cc := readMatchConsent(t, carol, "one"); cc.WithdrawnAt == nil || cc.FirstDeliveredAt == nil {
		t.Fatalf("withdrawn carol record must be retained while a clear is owed: %+v", cc)
	}
	// The person's view: the last answer carried only the country, while the
	// union still names the cleared real name.
	if ac = readMatchConsent(t, alice, "one"); *ac.LastDeliveredFields != (customerConsentFields{Country: true}) ||
		*ac.DeliveredFields != (customerConsentFields{RealName: true, Country: true}) {
		t.Fatalf("last delivered after provenance loss = %+v", ac)
	}

	// One hive cannot read another hive's consent, with either hive's key.
	resp = h.answer(h.match("two", "two", matchHash("one", "alice"), matchHash("two", "alice")))
	if len(resp.Matches) != 0 {
		t.Fatalf("hive two read hive one's consent: %+v", resp.Matches)
	}

	// Audit: hive id and counts only, never values or hashes.
	audit := h.logs.String()
	if !strings.Contains(audit, "audit: customer match") || !strings.Contains(audit, "hive=one") ||
		!strings.Contains(audit, "delivered=2") {
		t.Fatalf("missing match audit: %s", audit)
	}
	for _, leak := range []string{"Alice Real", "Carol Real", "FR", matchHash("one", "alice"), matchHash("one", "nobody-here")} {
		if strings.Contains(audit, leak) {
			t.Fatalf("audit leaked %q: %s", leak, audit)
		}
	}
}

func TestCustomerMatchLimits(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	h := newMatchHarness(t)
	hex := func(i int) string { return fmt.Sprintf("%064x", i) }
	status := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body)
		}
	}

	batch := make([]string, maxCustomerMatchBatch+1)
	for i := range batch {
		batch[i] = hex(i)
	}
	status(h.match("one", "one", batch...), http.StatusRequestEntityTooLarge)
	status(h.match("one", "one", batch[:maxCustomerMatchBatch]...), http.StatusOK)

	huge := `{"hive_id":"one","hashes":["` + strings.Repeat("a", maxCustomerMatchBody) + `"]}`
	status(h.post(h.s.heartbeatKeyFor("one"), huge), http.StatusRequestEntityTooLarge)
	status(h.match("one", "one", "ABC"), http.StatusBadRequest)
	status(h.match("one", "one", strings.ToUpper(hex(0xabc))), http.StatusBadRequest)
	status(h.post(h.s.heartbeatKeyFor("one"), `{"hive_id":"one","hashes":[],"emails":["a@b"]}`), http.StatusBadRequest)

	// Per-hive rate limit; another hive keeps its own budget.
	resetCustomerMatchRates()
	for i := 0; i < customerMatchRateLimit; i++ {
		status(h.match("one", "one"), http.StatusOK)
	}
	w := h.match("one", "one")
	status(w, http.StatusTooManyRequests)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	status(h.match("two", "two"), http.StatusOK)

	// Response size cap keeps a prefix and reports truncation.
	pending := []pendingCustomerMatch{
		{entry: customerMatchEntry{MatchHash: hex(1), RealName: strings.Repeat("x", 40)}},
		{entry: customerMatchEntry{MatchHash: hex(2), RealName: strings.Repeat("y", 40)}},
		{entry: customerMatchEntry{MatchHash: hex(3), RealName: strings.Repeat("z", 40)}},
	}
	one, _ := json.Marshal(pending[0].entry)
	limit := len(`{"matches":[],"truncated":true}`) + 2*(len(one)+1)
	kept, truncated := fitCustomerMatches(pending, limit)
	if len(kept) != 2 || !truncated {
		t.Fatalf("fit kept %d truncated=%v", len(kept), truncated)
	}
	if kept, truncated = fitCustomerMatches(pending, maxCustomerMatchResponse); len(kept) != 3 || truncated {
		t.Fatalf("fit under cap kept %d truncated=%v", len(kept), truncated)
	}
}

// A grant made through the consent page is matched at once, not after the
// index cache expires.
func TestCustomerMatchGrantInvalidatesIndex(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.example.com")
	h := newMatchHarness(t)
	saveMatchUser(t, &SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"one": "viewer"},
		FullName: "Alice Real", FullNameSource: countrySourceUser})
	if resp := h.answer(h.match("one", "one", matchHash("one", "alice"))); len(resp.Matches) != 0 {
		t.Fatalf("matched before consent: %+v", resp.Matches)
	}
	r := reqWithUser(http.MethodPost, consentTestAPI+"/one",
		`{"text_version":"`+customerConsentTextVersion+`","fields":{"real_name":true}}`, "alice")
	r.Header.Set("Origin", "https://hub.example.com")
	w := httptest.NewRecorder()
	h.s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("grant status %d: %s", w.Code, w.Body)
	}
	resp := h.answer(h.match("one", "one", matchHash("one", "alice")))
	if len(resp.Matches) != 1 || resp.Matches[0].RealName != "Alice Real" {
		t.Fatalf("grant not visible to the match index: %+v", resp.Matches)
	}
}
