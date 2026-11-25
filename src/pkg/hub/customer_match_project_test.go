package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The project-hive list: unset is the default project hive, "none" or an
// empty value answers no hive, invalid ids are dropped.
func TestCustomersProjectHivesParse(t *testing.T) {
	t.Setenv(customersProjectHivesEnv, "restored-after-test")
	if err := os.Unsetenv(customersProjectHivesEnv); err != nil {
		t.Fatal(err)
	}
	if got := customersProjectHives(); len(got) != 1 || !got[defaultCustomersProjectHive] {
		t.Fatalf("unset list = %v, want only the default project hive", got)
	}

	cases := map[string][]string{
		"":                     nil,
		"none":                 nil,
		"alpha, NONE, beta":    nil,
		"alpha, beta":          {"alpha", "beta"},
		" alpha ,, Bad Id!,":   {"alpha"},
		"alpha,alpha":          {"alpha"},
		"../escape,alpha-hive": {"alpha-hive"},
	}
	for raw, want := range cases {
		got := parseCustomersProjectHives(raw)
		if len(got) != len(want) {
			t.Errorf("parse(%q) = %v, want %v", raw, got, want)
			continue
		}
		for _, id := range want {
			if !got[id] {
				t.Errorf("parse(%q) = %v, missing %q", raw, got, id)
			}
		}
	}

	t.Setenv(customersProjectHivesEnv, "")
	if customersProjectHive(defaultCustomersProjectHive) {
		t.Fatal("an explicitly empty list must answer no hive, not the default")
	}
	t.Setenv(customersProjectHivesEnv, "alpha")
	if !customersProjectHive("alpha") || customersProjectHive(defaultCustomersProjectHive) || customersProjectHive("") {
		t.Fatal("a configured list must replace the default")
	}
}

// postMatch posts a match as hiveID, with or without the labels header, and
// returns the raw recorder so a refusal can be inspected.
func (h *matchHarness) postMatch(hiveID string, labeled bool, hashes ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	resetCustomerMatchRates()
	b, err := json.Marshal(customerMatchRequest{HiveID: hiveID, Hashes: hashes})
	if err != nil {
		h.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, customerMatchPath, strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+h.s.heartbeatKeyFor(hiveID))
	if labeled {
		r.Header.Set(customerMatchLabelsHeader, customerMatchLabelsV1)
	}
	w := httptest.NewRecorder()
	h.s.mux.ServeHTTP(w, r)
	return w
}

// A project hive hears what the hub knows about every person, labeled, with
// or without a grant on it; any other registered hive, even one that holds
// the person's consent and grant and asks for labels, is refused with no
// data and no consent stamp.
func TestCustomerMatchOnlyProjectHive(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	h := newMatchHarness(t)
	t.Setenv(customersProjectHivesEnv, "project")

	alice := saveMatchUser(t, &SaaSUser{GitHubUsername: "alice",
		Hives:    map[string]string{"project": "viewer", "other": "viewer"},
		FullName: "Alice Real", FullNameSource: countrySourceUser,
		Company: "Alice Co", CompanySource: countrySourceUser, Country: "GB", CountrySource: countrySourceUser})
	for _, hive := range []string{"project", "other"} {
		writeMatchConsent(t, alice, hive, func(c *customerConsent) {
			c.Fields = customerConsentFields{RealName: true, Organization: true, Country: true}
		})
	}
	// No grant on any hive; country inferred, organization from the hub's
	// affiliation lookup.
	saveMatchUser(t, &SaaSUser{GitHubUsername: "carol", Country: "JP", CountrySource: countrySourceInferred,
		Affiliation: "Carol Org", AffiliationSource: affiliationSourceOrgMembership})
	// A contributions affiliation names a repository, never an organization.
	saveMatchUser(t, &SaaSUser{GitHubUsername: "dave", Country: "BR", CountrySource: countrySourceUser,
		Affiliation: "some-repo-owner", AffiliationSource: affiliationSourceContributions})

	got := matchByHash(h.matchLabeled("project",
		matchHash("project", "alice"), matchHash("project", "carol"), matchHash("project", "dave")))
	if e := got[matchHash("project", "alice")]; e.RealName != "Alice Real" || e.Country != "GB" || e.Organization != "Alice Co" {
		t.Fatalf("project hive, consented grant holder: %+v", e)
	}
	if e := got[matchHash("project", "carol")]; e.Country != "JP" || e.Sources["country"] != customerMatchSourceBrowserLanguage ||
		e.Organization != "Carol Org" || e.Sources["organization"] != customerMatchSourceOrgMembership || e.RealName != "" {
		t.Fatalf("project hive, no grant: %+v", e)
	}
	if e := got[matchHash("project", "dave")]; e.Country != "BR" || e.Organization != "" || e.Sources["organization"] != "" {
		t.Fatalf("project hive, contributions affiliation: %+v", e)
	}

	for _, labeled := range []bool{true, false} {
		w := h.postMatch("other", labeled, matchHash("other", "alice"), matchHash("other", "carol"))
		if w.Code != http.StatusForbidden {
			t.Fatalf("other hive (labels %v): status %d, want 403: %s", labeled, w.Code, w.Body)
		}
		for _, leak := range []string{"Alice", "GB", "JP", "Carol", "match_hash"} {
			if strings.Contains(w.Body.String(), leak) {
				t.Fatalf("other hive (labels %v): %q crossed: %s", labeled, leak, w.Body)
			}
		}
	}
	if c := readMatchConsent(t, alice, "other"); c.FirstDeliveredAt != nil || c.DeliveredFields != nil {
		t.Fatalf("a refused hive must not be stamped as delivered: %+v", c)
	}
	if !strings.Contains(h.logs.String(), "audit: customer match refused") {
		t.Fatalf("refusal not audited: %s", h.logs)
	}

	// The bearer is still checked first: a bearer for another hive is 401.
	resetCustomerMatchRates()
	if w := h.match("project", "other", matchHash("other", "alice")); w.Code != http.StatusUnauthorized {
		t.Fatalf("mismatched bearer: status %d, want 401", w.Code)
	}
}
