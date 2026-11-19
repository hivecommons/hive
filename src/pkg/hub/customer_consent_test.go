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
