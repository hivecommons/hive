package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const consentTestAPI = "/api/saas/me/profile-share/consents"

func TestCustomerConsentRoutes(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "")
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.example.com")
	s := newHandlerHub()
	s.mux = http.NewServeMux()
	s.registerCustomerSharingRoutes()
	s.mux.HandleFunc("DELETE /admin/{username}", s.requireAdmin(s.handleAdminDeleteUser))
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))
	mkUser(t, hubAdminUsername)
	mkUser(t, "bob")
	alice := &SaaSUser{
		GitHubUsername: "alice", FullName: "Alice Private",
		Hives:      map[string]string{"one": "viewer", "two": "viewer", "expired": "viewer"},
		HiveExpiry: map[string]string{"expired": "2020-01-01T00:00:00Z"},
	}
	if err := saveSaaSUser(alice); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, login, origin string, impersonate bool) *httptest.ResponseRecorder {
		t.Helper()
		r := reqWithUser(method, path, body, login)
		if login == "" {
			r.Header.Del("Cookie")
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if impersonate {
			r.AddCookie(&http.Cookie{Name: impersonateCookieName, Value: mintImpersonateCookieValueForGeneration(s.currentGenerations(), login, "alice", time.Now())})
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}
	const hubOrigin = "https://hub.example.com"
	expect := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body)
		}
	}
	for _, path := range []string{"/profile-share", "/api/saas/me/profile-share", consentTestAPI} {
		expect(request("GET", path, "", "alice", "", false), 404)
	}
	t.Setenv(customersShareEnv, "true")
	expect(request("GET", "/profile-share", "", "", "", false), 303)
	expect(request("GET", consentTestAPI, "", "", "", false), 401)
	expect(request("GET", consentTestAPI, "", hubAdminUsername, "", true), 403)
	page := request("GET", "/profile-share", "", "alice", "", false)
	expect(page, 200)
	if !strings.Contains(page.Body.String(), `data-text-version="`+customerConsentTextVersion+`"`) || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("page did not bind the displayed disclosure version or disable caching")
	}

	grant := `{"text_version":"` + customerConsentTextVersion + `","fields":{"real_name":true}}`
	post := func(hive, body, login string, status int) {
		t.Helper()
		expect(request("POST", consentTestAPI+"/"+hive, body, login, hubOrigin, false), status)
	}
	post("one", strings.Replace(grant, `"fields"`, `"person":"bob","fields"`, 1), "alice", 400)
	post("one", strings.Replace(grant, customerConsentTextVersion, "old", 1), "alice", 409)
	post("one", strings.Replace(grant, "true", "false", 1), "alice", 400)
	post("one", grant, "bob", 403)
	post("one", grant, hubAdminUsername, 403)
	post("expired", grant, "alice", 403)
	expect(request("POST", consentTestAPI+"/one", grant, "alice", "https://other.example.com", false), 403)
	expect(request("POST", consentTestAPI+"/one", grant, hubAdminUsername, hubOrigin, true), 403)
	post("one", grant, "alice", 204)
	post("two", grant, "alice", 204)
	post("one", strings.Replace(grant, "real_name", "country", 1), "alice", 204)

	path, err := customerConsentPath(alice)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("consent permissions = %o", mode)
	}
	read := func(login string) []customerConsentHive {
		t.Helper()
		w := request("GET", consentTestAPI, "", login, "", false)
		expect(w, 200)
		var body struct{ Hives []customerConsentHive }
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Hives
	}
	if rows := read("bob"); len(rows) != 0 {
		t.Fatalf("another session saw consent rows: %+v", rows)
	}
	if rows := read("alice"); len(rows) != 2 || rows[0].Consent == nil || rows[1].Consent == nil ||
		rows[0].Consent.Fields != (customerConsentFields{Country: true}) || rows[0].Consent.Person != "alice" ||
		rows[1].Consent.Fields != (customerConsentFields{RealName: true}) {
		t.Fatalf("expected independent replacement and expired grant filtering, got %+v", rows)
	}
	expect(request("DELETE", consentTestAPI+"/one", "", "alice", hubOrigin, false), 204)
	if rows := read("alice"); len(rows) != 2 || rows[0].Consent != nil || rows[1].Consent == nil {
		t.Fatalf("withdrawal must delete only the undelivered hive record: %+v", rows)
	}
	if audit := logs.String(); strings.Count(audit, "audit: customer consent") != 4 || strings.Contains(audit, "Alice Private") {
		t.Fatalf("unexpected consent audit: %s", audit)
	}
	t.Setenv(customersShareEnv, "off")
	expect(request("DELETE", "/admin/alice", "", hubAdminUsername, hubOrigin, false), 200)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || loadSaaSUser("alice") != nil {
		t.Fatalf("user deletion must remove profile and all consents even when sharing is off: %v", err)
	}
}

func TestCustomerConsentRetention(t *testing.T) {
	defer helperSetupTempDirs(t)()
	now := time.Now().UTC()
	old := now.Add(-customerConsentRetention)
	path, err := customerConsentPath(&SaaSUser{GitHubUsername: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveCustomerConsents(path, map[string]customerConsent{
		"undelivered": {WithdrawnAt: &now},
		"expired":     {WithdrawnAt: &old, FirstDeliveredAt: &old},
		"retained":    {WithdrawnAt: &now, FirstDeliveredAt: &old},
		"active":      {FirstDeliveredAt: &old},
	}); err != nil {
		t.Fatal(err)
	}
	if err := pruneCustomerConsents(path, now); err != nil {
		t.Fatal(err)
	}
	records, err := readCustomerConsents(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records["active"].FirstDeliveredAt == nil || records["retained"].WithdrawnAt == nil {
		t.Fatalf("unexpected retention result: %+v", records)
	}
}

func TestCustomerConsentDisclosureText(t *testing.T) {
	page := profileSharePageHTML
	for _, want := range []string{
		"What is sent.", "To whom.", "can view and export", "What is never sent.",
		"Withdrawing.", "at its next sync", "cannot recall", "learns that this hive has you as a customer",
		`class="muted delivery"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("disclosure is missing %q", want)
		}
	}
	if strings.Contains(page, "No profile details are sent") {
		t.Error("disclosure still says nothing is sent to hives")
	}
	if customerConsentTextVersion == "1" {
		t.Error("the disclosure changed materially; a version 1 consent must be reconfirmed")
	}
}

func TestCustomerConsentAdminCounts(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "")
	s := newHandlerHub()
	s.mux = http.NewServeMux()
	s.registerCustomerSharingRoutes()
	mkUser(t, hubAdminUsername)
	mkUser(t, "bob")
	now := time.Now().UTC()
	write := func(login string, records map[string]customerConsent) {
		t.Helper()
		u := &SaaSUser{GitHubUsername: login, FullName: "Private " + login, FullNameSource: countrySourceUser}
		if err := saveSaaSUser(u); err != nil {
			t.Fatal(err)
		}
		path, err := customerConsentPath(u)
		if err != nil {
			t.Fatal(err)
		}
		if err := saveCustomerConsents(path, records); err != nil {
			t.Fatal(err)
		}
	}
	name := customerConsentFields{RealName: true}
	current := func(f customerConsentFields) customerConsent {
		return customerConsent{Fields: f, TextVersion: customerConsentTextVersion, GrantedAt: now, UpdatedAt: now}
	}
	old := current(name)
	old.TextVersion = "1"
	withdrawn := customerConsent{TextVersion: customerConsentTextVersion, WithdrawnAt: &now, FirstDeliveredAt: &now}
	write("alice", map[string]customerConsent{"one": current(name), "two": current(customerConsentFields{Country: true})})
	write("carol", map[string]customerConsent{"one": current(customerConsentFields{Organization: true}), "two": withdrawn})
	write("dave", map[string]customerConsent{"one": old})

	get := func(login string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, reqWithUser("GET", "/api/saas/admin/profile-share/counts", "", login))
		return w
	}
	if w := get(hubAdminUsername); w.Code != http.StatusNotFound {
		t.Fatalf("switch off: status %d, want 404", w.Code)
	}
	t.Setenv(customersShareEnv, "true")
	if w := get("bob"); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin: status %d, want 403", w.Code)
	}
	w := get(hubAdminUsername)
	if w.Code != http.StatusOK {
		t.Fatalf("admin: status %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, leak := range []string{"alice", "carol", "dave", "Private", "real_name", "organization", "country\"", "fields"} {
		if strings.Contains(body, leak) {
			t.Fatalf("admin counts leaked %q: %s", leak, body)
		}
	}
	var resp struct {
		Hives []customerConsentHiveCount `json:"hives"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := []customerConsentHiveCount{
		{HiveID: "one", Name: "one", Consents: 2, AwaitingReconfirmation: 1},
		{HiveID: "two", Name: "two", Consents: 1},
	}
	if len(resp.Hives) != len(want) || resp.Hives[0] != want[0] || resp.Hives[1] != want[1] {
		t.Fatalf("counts = %+v, want %+v", resp.Hives, want)
	}
}
