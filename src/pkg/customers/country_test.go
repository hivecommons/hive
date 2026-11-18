package customers

import (
	"context"
	"testing"
)

func TestDeriveCountry(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Carmel, New York", "US"},
		{"Berlin", "DE"},
		{"São Paulo, Brazil", "BR"},
		{"Sao Paulo", "BR"},
		{"UK", "GB"},
		{"U.K.", "GB"},
		{"London, England", "GB"},
		{"remote", ""},
		{"Remote", ""},
		{"Earth", ""},
		{"", ""},
		{"   ", ""},
		{"Remote, UK", "GB"},
		{"Austin, TX", "US"},
		{"San Francisco, CA", "US"},
		{"Toronto, ON", "CA"},
		{"Toronto, CA", "CA"},
		{"Berlin, DE", "DE"},
		{"Wilmington, DE", "US"},
		{"Tbilisi, Georgia", "GE"},
		{"Atlanta, Georgia", "US"},
		{"Georgia", "US"},
		{"Paris, Texas", "US"},
		{"Paris", "FR"},
		{"London, Ontario", "CA"},
		{"Munich Germany", "DE"},
		{"Kansas City", "US"},
		{"USA", "US"},
		{"U.S.A.", "US"},
		{"The Netherlands", "NL"},
		{"Bengaluru, Karnataka, India", "IN"},
		{"IN", "IN"},
		{"Zürich, Schweiz", "CH"},
		{"Berlin \U0001F1E9\U0001F1EA", "DE"},
		{"\U0001F1EF\U0001F1F5", "JP"},
		{"Come visit us", ""},
		{"Somewhere over the rainbow", ""},
		{"Worldwide", ""},
		{"Cambridge, MA", "US"},
		{"San Francisco Bay Area", "US"},
	}
	for _, c := range cases {
		if got := DeriveCountry(c.in); got != c.want {
			t.Errorf("DeriveCountry(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCountryTableIsConsistent(t *testing.T) {
	if len(countryByCode) < 240 {
		t.Fatalf("only %d countries in the table", len(countryByCode))
	}
	for name, code := range countryNames {
		if !ValidCountry(code) || name != foldPlace(name) && name != "uk" {
			t.Errorf("country entry %q -> %q is not folded or not an ISO code", name, code)
		}
	}
	for tbl, m := range map[string]map[string]string{"subdivision": subdivisions, "city": cityCountries, "abbrev": stateAbbrevMap} {
		for name, code := range m {
			if !ValidCountry(code) {
				t.Errorf("%s %q -> %q is not an ISO code", tbl, name, code)
			}
			if tbl != "abbrev" && name != foldPlace(name) {
				t.Errorf("%s %q is not folded", tbl, name)
			}
		}
	}
}

func TestCountryFromInput(t *testing.T) {
	for in, want := range map[string]string{"de": "DE", "DE": "DE", "Germany": "DE", "uk": "GB", "United Kingdom": "GB", "": "", "Atlantis": "", "zz": "", "France ": "FR"} {
		if got := CountryFromInput(in); got != want {
			t.Errorf("CountryFromInput(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCountryProvenanceAndNeverOverwrite(t *testing.T) {
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := mustAdd(t, st, "ada", adaLogin)

	// Location by hand derives an automatic country with its rule.
	p, err = st.SetProfileField(p.ID, FieldLocation, "Berlin", "olivia")
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Profile.Country; c.Value != "DE" || c.Source != SourceAuto || c.Rule != CountryRuleLocation {
		t.Fatalf("derived country = %+v", c)
	}
	// A moved location follows; an unknown one drops the guess.
	p, _ = st.SetProfileField(p.ID, FieldLocation, "Lyon", "olivia")
	if p.Profile.Country.Value != "FR" {
		t.Fatalf("country after move = %+v", p.Profile.Country)
	}
	p, _ = st.SetProfileField(p.ID, FieldLocation, "remote", "olivia")
	if p.Profile.Country.Value != "" || p.Profile.Country.Source != "" {
		t.Fatalf("country kept after an unplaceable location = %+v", p.Profile.Country)
	}

	// A hand-set country is never changed by the location or by GitHub.
	p, err = st.SetProfileField(p.ID, FieldCountry, "japan", "olivia")
	if err != nil || p.Profile.Country.Value != "JP" || p.Profile.Country.Source != SourceManual {
		t.Fatalf("manual country = %+v, %v", p.Profile.Country, err)
	}
	p, _ = st.SetProfileField(p.ID, FieldLocation, "Berlin", "olivia")
	p, changed, err := st.ApplyAutoProfile(p.ID, map[string]string{FieldLocation: "Paris"})
	if err != nil || p.Profile.Country.Value != "JP" {
		t.Fatalf("country after fetch = %+v, %v (changed=%v)", p.Profile.Country, err, changed)
	}
	// Clearing by hand is honored: it stays empty and manual.
	p, _ = st.SetProfileField(p.ID, FieldCountry, "", "olivia")
	if p.Profile.Country.Value != "" || p.Profile.Country.Source != SourceManual {
		t.Fatalf("cleared country = %+v", p.Profile.Country)
	}
	p, _ = st.SetProfileField(p.ID, FieldLocation, "Madrid", "olivia")
	if p.Profile.Country.Value != "" {
		t.Fatalf("a cleared country was refilled: %+v", p.Profile.Country)
	}
	// Auto hands it back and derives again from the location now held.
	p, _ = st.ResetProfileField(p.ID, FieldCountry)
	if p.Profile.Country.Value != "ES" || p.Profile.Country.Source != SourceAuto {
		t.Fatalf("country after Auto = %+v", p.Profile.Country)
	}
	// Garbage is refused, not stored.
	if _, err := st.SetProfileField(p.ID, FieldCountry, "Atlantis", "olivia"); err == nil {
		t.Fatal("an unknown country was accepted")
	}
	// Resetting the location drops the country it implied.
	p, _ = st.ResetProfileField(p.ID, FieldLocation)
	if p.Profile.Country.Value != "" {
		t.Fatalf("country kept after the location was reset: %+v", p.Profile.Country)
	}
}

func TestFetchProfileDerivesCountry(t *testing.T) {
	x, _ := profileFixture(t)
	x.m.FetchProfile(context.Background(), x.person.ID, adaLogin)
	p, _ := x.m.Store.Get(x.person.ID)
	if c := p.Profile.Country; c.Value != "GB" || c.Source != SourceAuto || c.Rule != CountryRuleLocation {
		t.Fatalf("country from the GitHub location = %+v", c)
	}
}
