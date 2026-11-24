package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requestMyProfileShare(s *HubServer, method, user, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleMyProfileShare(rec, reqWithUser(method, "/api/saas/me/profile-share", body, user))
	return rec
}

func TestMyProfileShareProvenance(t *testing.T) {
	s := adminAssignHub(t)
	u := &SaaSUser{GitHubUsername: "ada", Hives: map[string]string{}, FullName: "Legacy", Country: "FR", DisplayName: "Provider Name", Affiliation: "Public affiliation"}
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	assertProfile := func(method, request, want string) {
		t.Helper()
		rec := requestMyProfileShare(s, method, "ada", request)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("%s %s returned %d %s; want 200 %s", method, request, rec.Code, rec.Body.String(), want)
		}
	}
	assertProfile(http.MethodGet, "", `{"real_name":{"value":"Legacy","source":""},"organization":{"value":"","source":""},"country":{"value":"FR","source":"inferred"}}`)

	if rec := adminPut(s, "ada", `{"full_name":" Admin Name ","company":" Admin Org "}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("admin update returned %d %s", rec.Code, rec.Body.String())
	}
	assertProfile(http.MethodGet, "", `{"real_name":{"value":"Admin Name","source":"admin"},"organization":{"value":"Admin Org","source":"admin"},"country":{"value":"FR","source":"inferred"}}`)
	assertProfile(http.MethodPut, `{"real_name":" Ada ","country":" gb "}`, `{"real_name":{"value":"Ada","source":"user"},"organization":{"value":"Admin Org","source":"admin"},"country":{"value":"GB","source":"user"}}`)
	const cleared = `{"real_name":{"value":"","source":"user"},"organization":{"value":"","source":"user"},"country":{"value":"","source":"user"}}`
	assertProfile(http.MethodPut, `{"real_name":"  ","organization":"","country":""}`, cleared)
	if rec := adminPut(s, "ada", `{"full_name":"Replaced","company":"Replaced","country":"US","saas_quota":7}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("admin update returned %d %s", rec.Code, rec.Body.String())
	}
	assertProfile(http.MethodGet, "", cleared)
	u = loadSaaSUser("ada")
	if u.SaaSQuota != 7 || !u.CountrySetByUser || u.DisplayName != "Provider Name" || u.Affiliation != "Public affiliation" {
		t.Fatalf("unexpected saved record: %+v", u)
	}
}

func TestMyProfileShareValidationAndCaps(t *testing.T) {
	s := countryTestHub(t)
	mkUser(t, "ada")
	for _, body := range []string{
		`null`, `[]`, `{`, `{} {}`, `{"real_name":null}`, `{"organization":123}`, `{"country":"USA","real_name":"Ada"}`,
		`{"owner":"someone","real_name":"Ada"}`, `{"person":"someone"}`, `{"Real_Name":"Ada"}`,
		`{"real_name":"Ada","real_name":"Other"}`,
		`{"organization":"` + strings.Repeat("x", maxUpdateUserBodyBytes) + `"}`,
	} {
		rec := requestMyProfileShare(s, http.MethodPut, "ada", body)
		var reply struct {
			Error string `json:"error"`
		}
		if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &reply) != nil || reply.Error == "" {
			t.Fatalf("invalid profile update returned %d %s; want a JSON 400 error", rec.Code, rec.Body.String())
		}
	}
	u := loadSaaSUser("ada")
	if u.FullNameSource != "" || u.CompanySource != "" || u.CountrySource != "" {
		t.Fatalf("invalid update changed profile: %+v", u)
	}
	name := strings.Repeat("é", maxContactNameLen)
	org := strings.Repeat("語", maxContactCompanyLen)
	body, err := json.Marshal(map[string]string{"real_name": " " + name + "X ", "organization": " " + org + "X "})
	if err != nil {
		t.Fatal(err)
	}
	if rec := requestMyProfileShare(s, http.MethodPut, "ada", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("profile update returned %d %s", rec.Code, rec.Body.String())
	}
	u = loadSaaSUser("ada")
	if u.FullName != name || u.Company != org || u.FullNameSource != countrySourceUser || u.CompanySource != countrySourceUser {
		t.Fatalf("profile fields not capped and attributed correctly: %+v", u)
	}
}

func TestProfileShareLegacyFileRoundTrip(t *testing.T) {
	s := countryTestHub(t)
	const legacy = `{"github_username":"ada","created_at":"2020-01-01T00:00:00Z","last_login":"2020-01-02T00:00:00Z","hives":{},"saas_quota":1,"blocked":false,"full_name":"Ada","slack_id":"U123","notes":"private","company":"Legacy Org","country":"GB","country_set_by_user":true,"login_count":1}`
	var raw bytes.Buffer
	if err := json.Indent(&raw, []byte(legacy), "", "  "); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(saasUsersDir, "ada.json")
	if err := os.MkdirAll(saasUsersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	u := loadSaaSUser("ada")
	if u == nil || u.FullNameSource != "" || u.CompanySource != "" || u.CountrySource != "" {
		t.Fatalf("legacy sources backfilled: %+v", u)
	}
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, raw.Bytes()) {
		t.Fatalf("legacy file changed: %v\ngot %s\nwant %s", err, stored, raw.Bytes())
	}
	const want = `{"real_name":{"value":"Ada","source":""},"organization":{"value":"Legacy Org","source":""},"country":{"value":"GB","source":"user"}}`
	if rec := requestMyProfileShare(s, http.MethodGet, "ada", ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("legacy profile returned %d %s; want 200 %s", rec.Code, rec.Body.String(), want)
	}
}

func TestProfileShareProvisionApproval(t *testing.T) {
	t.Cleanup(helperSetupTempDirs(t))
	mkUser(t, hubAdminUsername)
	mkUser(t, "ada")
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, saveCh: make(chan struct{}, 1), clusters: map[string]ClusterConfig{"hive-oke": *dynamicCluster()}}
	if err := saveProvisionRequest(&ProvisionRequest{
		Username: "ada", Org: "repo-owner", Repos: "repo", PrimaryRepo: "repo", ACMMLevel: 2, AuthMethod: "public",
		Status: provisionStatusPending, FullName: " Ada Lovelace ", Country: "gb",
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveSaaSHive(&SaaSHive{ID: "ph1", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleApproveProvision(rec, setPathValue(reqWithUser(http.MethodPost, "/approve-prov", "", hubAdminUsername), "username", "ada"))
	if rec.Code != http.StatusOK {
		t.Fatalf("approval returned %d %s", rec.Code, rec.Body.String())
	}
	u := loadSaaSUser("ada")
	if u == nil || u.FullName != "Ada Lovelace" || u.FullNameSource != countrySourceUser || u.Country != "GB" || u.CountrySource != countrySourceUser || u.Company != "" || u.CompanySource != "" {
		t.Fatalf("approval did not attribute only the supplied contact fields: %+v", u)
	}
}

func TestProfileShareApprovalPreservesExplicitClears(t *testing.T) {
	u := &SaaSUser{FullNameSource: countrySourceUser, CountrySource: countrySourceUser, CountrySetByUser: true}
	applyRequestContactToUser(u, &ProvisionRequest{FullName: "Earlier", Country: "GB"})
	if u.FullName != "" || u.Country != "" || u.FullNameSource != countrySourceUser || u.CountrySource != countrySourceUser {
		t.Fatalf("approval overwrote a user clear: %+v", u)
	}
	u = &SaaSUser{}
	applyRequestContactToUser(u, &ProvisionRequest{FullName: "  "})
	if u.FullNameSource != "" {
		t.Fatalf("approval attributed blank request name: %+v", u)
	}
}
