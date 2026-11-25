package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/customers"
)

// matchLabeled posts a match as hiveID, asking for labeled public fields.
func (h *matchHarness) matchLabeled(hiveID string, hashes ...string) customerMatchResponse {
	h.t.Helper()
	resetCustomerMatchRates()
	b, err := json.Marshal(customerMatchRequest{HiveID: hiveID, Hashes: hashes})
	if err != nil {
		h.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, customerMatchPath, strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+h.s.heartbeatKeyFor(hiveID))
	r.Header.Set(customerMatchLabelsHeader, customerMatchLabelsV1)
	w := httptest.NewRecorder()
	h.s.mux.ServeHTTP(w, r)
	for _, secret := range []string{"secret-notes", "U0SLACK", "@", "ibm.com", "gmail"} {
		if strings.Contains(w.Body.String(), secret) {
			h.t.Fatalf("private value %q crossed: %s", secret, w.Body)
		}
	}
	return h.answer(w)
}

func matchByHash(resp customerMatchResponse) map[string]customerMatchEntry {
	out := make(map[string]customerMatchEntry, len(resp.Matches))
	for _, m := range resp.Matches {
		out[m.MatchHash] = m
	}
	return out
}

// Country and organization pass labeled to a project hive without consent,
// whether or not the person holds a grant on it; real name, notes, email,
// email domains and Slack ids do not. The hub holds no location for a
// person, so none can cross.
func TestCustomerMatchPublicFieldsLabeledWithoutConsent(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	t.Setenv(customersOrgDomainsEnv, "ibm.com=IBM, gmail.com=Google, clubco.example=")
	h := newMatchHarness(t)
	one := map[string]string{"one": "viewer"}
	private := func(u *SaaSUser) *SaaSUser {
		u.Notes, u.SlackID = "secret-notes", "U0SLACK"
		return saveMatchUser(t, u)
	}
	private(&SaaSUser{GitHubUsername: "admin-set", Hives: one, FullName: "Hidden Name", FullNameSource: countrySourceUser,
		Company: "Admin Co", CompanySource: countrySourceAdmin, Country: "GB", CountrySource: countrySourceAdmin})
	private(&SaaSUser{GitHubUsername: "self-set", Hives: one, Company: "Self Co", CompanySource: countrySourceUser,
		Country: "DE", CountrySource: countrySourceUser})
	private(&SaaSUser{GitHubUsername: "inferred", Hives: one, Country: "FR", CountrySource: countrySourceInferred})
	private(&SaaSUser{GitHubUsername: "legacy", Hives: one, Company: "Old Co"})
	private(&SaaSUser{GitHubUsername: "org-mail", Hives: one, Email: "pat@us.ibm.com"})
	private(&SaaSUser{GitHubUsername: "free-mail", Hives: one, Email: "pat@gmail.com"})
	private(&SaaSUser{GitHubUsername: "vanity-mail", Hives: one, Email: "pat@clubco.example"})
	private(&SaaSUser{GitHubUsername: "cleared-org", Hives: one, Email: "pat@ibm.com", CompanySource: countrySourceUser})
	private(&SaaSUser{GitHubUsername: "no-grant", Hives: map[string]string{"two": "viewer"}, Company: "Other Co", CompanySource: countrySourceUser})
	private(&SaaSUser{GitHubUsername: "blocked", Hives: one, Company: "Blocked Co", CompanySource: countrySourceUser, Blocked: true})

	logins := []string{"admin-set", "self-set", "inferred", "legacy", "org-mail", "free-mail", "vanity-mail", "cleared-org", "no-grant", "blocked"}
	hashes := make([]string, 0, len(logins))
	for _, l := range logins {
		hashes = append(hashes, matchHash("one", l))
	}

	// Without the labels header the answer is consent-only: nobody here
	// consented, so it is silence.
	resetCustomerMatchRates()
	if got := h.match("one", "one", hashes...).Body.String(); got != "{\"matches\":[]}\n" {
		t.Fatalf("unlabeled answer = %q, want silence", got)
	}

	got := matchByHash(h.matchLabeled("one", hashes...))
	want := map[string]struct{ org, orgSrc, country, countrySrc string }{
		"admin-set": {"Admin Co", customerMatchSourceAdmin, "GB", customerMatchSourceAdmin},
		"self-set":  {"Self Co", customerMatchSourcePerson, "DE", customerMatchSourcePerson},
		"inferred":  {"", "", "FR", customerMatchSourceBrowserLanguage},
		"legacy":    {"Old Co", customerMatchSourceUnrecorded, "", ""},
		"org-mail":  {"IBM", customerMatchSourceEmailDomain, "", ""},
		"no-grant":  {"Other Co", customerMatchSourcePerson, "", ""},
	}
	for login, w := range want {
		e, ok := got[matchHash("one", login)]
		if !ok {
			t.Fatalf("%s: no entry in %+v", login, got)
		}
		if e.RealName != "" {
			t.Fatalf("%s: real name crossed without consent: %+v", login, e)
		}
		if e.Organization != w.org || e.Sources["organization"] != w.orgSrc ||
			e.Country != w.country || e.Sources["country"] != w.countrySrc || e.Rev == "" {
			t.Fatalf("%s: entry %+v, want %+v", login, e, w)
		}
	}
	// A free-mail domain (even if listed), a listed domain with no name, an
	// explicit clear and a blocked person are silence.
	for _, login := range []string{"free-mail", "vanity-mail", "cleared-org", "blocked"} {
		if e, ok := got[matchHash("one", login)]; ok {
			t.Fatalf("%s: crossed: %+v", login, e)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("answer has %d entries, want %d", len(got), len(want))
	}
	// Public deliveries are not consent deliveries: no record is created.
	for _, login := range logins {
		u := loadSaaSUser(login)
		if u == nil {
			continue
		}
		if c := readMatchConsent(t, u, "one"); c.FirstDeliveredAt != nil || c.HiveID != "" {
			t.Fatalf("%s: consent record written for a public delivery: %+v", login, c)
		}
	}
}

// A private-domain email never yields an organization, whatever the list says.
func TestCustomerOrgFromEmailDomainDropsPrivate(t *testing.T) {
	domains := parseCustomerOrgDomains("ibm.com=IBM,gmail.com=Google,outlook.com=Microsoft,localhost=Local,=Nobody,redhat.com")
	if len(domains) != 1 || domains["ibm.com"] != "IBM" {
		t.Fatalf("parsed %v, want only ibm.com", domains)
	}
	cases := map[string]string{
		"a@ibm.com":           "IBM",
		"a@Research.IBM.com.": "IBM",
		"a@notibm.com":        "",
		"a@gmail.com":         "",
		"a@outlook.com":       "",
		"a@clubanderson.com":  "",
		"no-at-sign":          "",
		"":                    "",
	}
	for email, want := range cases {
		if got := orgFromEmailDomain(email, domains); got != want {
			t.Errorf("orgFromEmailDomain(%q) = %q, want %q", email, got, want)
		}
	}
	if got := orgFromEmailDomain("a@ibm.com", nil); got != "" {
		t.Fatalf("empty list derived %q", got)
	}
}

// With consent, real name still crosses, labeled as consented; withdrawing
// consent clears the real name but not the public fields.
func TestCustomerMatchPublicWithConsentAndWithdrawal(t *testing.T) {
	defer helperSetupTempDirs(t)()
	t.Setenv(customersShareEnv, "true")
	h := newMatchHarness(t)
	alice := saveMatchUser(t, &SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"one": "viewer"},
		FullName: "Alice Real", FullNameSource: countrySourceUser,
		Company: "Alice Co", CompanySource: countrySourceUser, Country: "GB", CountrySource: countrySourceAdmin})
	writeMatchConsent(t, alice, "one", func(c *customerConsent) {
		c.Fields = customerConsentFields{RealName: true, Organization: true, Country: true}
	})
	hash := matchHash("one", "alice")
	e := matchByHash(h.matchLabeled("one", hash))[hash]
	if e.RealName != "Alice Real" || e.Sources["real_name"] != customerMatchSourceConsent ||
		e.Organization != "Alice Co" || e.Sources["organization"] != customerMatchSourcePerson ||
		e.Country != "GB" || e.Sources["country"] != customerMatchSourceAdmin {
		t.Fatalf("consented entry = %+v", e)
	}
	if d := readMatchConsent(t, alice, "one").DeliveredFields; d == nil || !d.RealName {
		t.Fatalf("consent delivery not stamped: %+v", d)
	}

	now := time.Now().UTC()
	writeMatchConsent(t, alice, "one", func(c *customerConsent) {
		c.Fields, c.WithdrawnAt = customerConsentFields{}, &now
	})
	invalidateCustomerMatchIndex()
	e = matchByHash(h.matchLabeled("one", hash))[hash]
	if e.RealName != "" || len(e.Cleared) != 1 || e.Cleared[0] != "real_name" ||
		e.Organization != "Alice Co" || e.Country != "GB" {
		t.Fatalf("after withdrawal = %+v, want real_name cleared and public fields kept", e)
	}
}

// The spoke asks with the same header and understands every label the hub
// sends; a label it did not understand would be stored as consented.
func TestCustomerMatchLabelsSpokeParity(t *testing.T) {
	if customers.HubMatchLabelsHeader != customerMatchLabelsHeader || customers.HubMatchLabelsV1 != customerMatchLabelsV1 {
		t.Fatal("spoke and hub disagree on the labels header")
	}
	labels := []string{customerMatchSourcePerson, customerMatchSourceAdmin, customerMatchSourceEmailDomain,
		customerMatchSourceBrowserLanguage, customerMatchSourceUnrecorded}
	for _, src := range customerMatchAffiliationLabels {
		labels = append(labels, src)
	}
	for _, src := range labels {
		resp := customerMatchResponse{Matches: []customerMatchEntry{{
			MatchHash: strings.Repeat("a", customerMatchHashLen), Organization: "Org", Country: "GB",
			Sources: map[string]string{"organization": src, "country": src},
		}}}
		b, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := customers.DecodeHubMatchReply(bytes.NewReader(b))
		if err != nil || len(reply.Matches) != 1 {
			t.Fatalf("%s: decode %v %+v", src, err, reply)
		}
		rules := reply.Matches[0].Rules
		for _, field := range []string{customers.FieldOrganization, customers.FieldCountry} {
			if r := rules[field]; r == "" || r == customers.ProfileSourceHub {
				t.Fatalf("%s: %s rule %q, want a public hub rule", src, field, r)
			}
		}
	}
}
