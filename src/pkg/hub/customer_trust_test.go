package hub

import (
	"strings"
	"testing"
)

func TestParseCustomersTrustedHives(t *testing.T) {
	got := parseCustomersTrustedHives(" own-hive , ,bad/id, second-hive")
	if !got["own-hive"] || !got["second-hive"] || len(got) != 2 {
		t.Fatalf("parsed = %v", got)
	}
	if len(parseCustomersTrustedHives("")) != 0 {
		t.Fatal("an empty list must trust no hive")
	}
}

// The project's own hive (on the allowlist) hears the hub's labelled public
// fields and consented values; any other hive, with the same grant holders,
// a valid bearer, consent records and the labels header (its hub_import
// switch on), hears nothing.
func TestCustomerMatchTrustDomain(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	h := newMatchHarness(t)
	t.Setenv(customersTrustedHivesEnv, "own")

	alice := saveMatchUser(t, &SaaSUser{
		GitHubUsername: "alice", Hives: map[string]string{"own": "viewer", "third": "viewer"},
		FullName: "Alice Example", FullNameSource: countrySourceUser,
		Company: "Example Org", CompanySource: countrySourceUser,
		Country: "GB", CountrySource: countrySourceUser,
	})
	all := customerConsentFields{RealName: true, Organization: true, Country: true}
	writeMatchConsent(t, alice, "own", func(c *customerConsent) { c.Fields = all })
	writeMatchConsent(t, alice, "third", func(c *customerConsent) { c.Fields = all })

	own := matchByHash(h.matchLabeled("own", matchHash("own", "alice")))
	e, ok := own[matchHash("own", "alice")]
	if !ok || e.Organization != "Example Org" || e.Country != "GB" || e.RealName != "Alice Example" {
		t.Fatalf("own hive answer = %+v", own)
	}
	if e.Sources["organization"] != customerMatchSourcePerson {
		t.Fatalf("own hive values must stay labelled: %+v", e.Sources)
	}

	third := h.matchLabeled("third", matchHash("third", "alice"))
	if len(third.Matches) != 0 {
		t.Fatalf("a hive outside the trust domain heard: %+v", third.Matches)
	}
	resetCustomerMatchRates()
	if body := h.match("third", "third", matchHash("third", "alice")).Body.String(); body != "{\"matches\":[]}\n" {
		t.Fatalf("unlabelled answer to an untrusted hive = %q", body)
	}
	if readMatchConsent(t, alice, "third").DeliveredFields != nil {
		t.Fatal("an untrusted hive's consent record was stamped as delivered")
	}
	if !strings.Contains(h.logs.String(), "trusted=false") {
		t.Fatalf("untrusted request not audited: %s", h.logs)
	}

	// With the allowlist empty, even the project's own hive hears nothing.
	t.Setenv(customersTrustedHivesEnv, "")
	if got := h.matchLabeled("own", matchHash("own", "alice")); len(got.Matches) != 0 {
		t.Fatalf("an empty allowlist must trust no hive: %+v", got.Matches)
	}
}
