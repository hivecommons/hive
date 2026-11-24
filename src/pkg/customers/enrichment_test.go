package customers

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var enrichAt = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func openEnrichment(t *testing.T) *EnrichmentStore {
	t.Helper()
	e, err := OpenEnrichment(filepath.Join(t.TempDir(), "customers"))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEnrichmentStoreRoundTripAndMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "customers")
	e, err := OpenEnrichment(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Get("p_1"); ok {
		t.Fatal("an empty store holds nothing")
	}
	if err := e.SetSource("p_1", EnrichSourceGitdm, []EnrichedAffiliation{
		{Org: " Acme ", From: "2020-01-01", Until: "2022-06-30"},
		{Org: "Globex", From: "2022-07-01"},
		{Org: "bad date", From: "July"},
		{Org: ""},
	}, nil, enrichAt); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(e.Path())
	if err != nil || fi.Mode().Perm() != storeFileMode {
		t.Fatalf("enrichment file: %v, %v", fi, err)
	}
	again, err := OpenEnrichment(dir)
	if err != nil {
		t.Fatal(err)
	}
	pe, ok := again.Get("p_1")
	if !ok || len(pe.Affiliations) != 2 || pe.Affiliations[0].Org != "Acme" || pe.Affiliations[0].Source != EnrichSourceGitdm ||
		!pe.FetchedAt[EnrichSourceGitdm].Equal(enrichAt) {
		t.Fatalf("reopened = %+v", pe)
	}
	// Replacing one source keeps the others.
	if err := again.SetSource("p_1", EnrichSourceDevstats, nil, []EnrichedActivity{
		{Project: "kubernetes", Contributions: 12, URL: "https://devstats.cncf.io/x"},
		{Project: "evil", Contributions: 1, URL: "javascript:alert(1)"},
		{Project: "neg", Contributions: -1},
	}, enrichAt); err != nil {
		t.Fatal(err)
	}
	if err := again.SetSource("p_1", EnrichSourceGitdm, []EnrichedAffiliation{{Org: "Initech"}}, nil, enrichAt); err != nil {
		t.Fatal(err)
	}
	pe, _ = again.Get("p_1")
	if len(pe.Affiliations) != 1 || pe.Affiliations[0].Org != "Initech" || len(pe.Activity) != 2 || pe.Activity[1].URL != "" {
		t.Fatalf("after replace = %+v", pe)
	}
	if err := again.SetSource("", EnrichSourceGitdm, nil, nil, enrichAt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no person = %v", err)
	}
}

func TestEnrichmentStoreCapsEntries(t *testing.T) {
	e := openEnrichment(t)
	var affs []EnrichedAffiliation
	for i := 0; i < MaxEnrichedAffiliations+5; i++ {
		affs = append(affs, EnrichedAffiliation{Org: "Org"})
	}
	if err := e.SetSource("p_1", EnrichSourceGitdm, affs, nil, enrichAt); err != nil {
		t.Fatal(err)
	}
	if pe, _ := e.Get("p_1"); len(pe.Affiliations) != MaxEnrichedAffiliations {
		t.Fatalf("kept %d affiliations, want %d", len(pe.Affiliations), MaxEnrichedAffiliations)
	}
}

func TestEnrichmentStoreDropPruneDelete(t *testing.T) {
	e := openEnrichment(t)
	_ = e.SetSource("p_1", EnrichSourceGitdm, []EnrichedAffiliation{{Org: "Acme"}}, nil, enrichAt)
	_ = e.SetSource("p_1", EnrichSourceDevstats, nil, []EnrichedActivity{{Project: "k8s", Contributions: 1}}, enrichAt)
	_ = e.SetSource("p_2", EnrichSourceGitdm, []EnrichedAffiliation{{Org: "Globex"}}, nil, enrichAt)
	if n, err := e.DropSource(EnrichSourceGitdm); err != nil || n != 2 {
		t.Fatalf("DropSource = %d, %v", n, err)
	}
	if _, ok := e.Get("p_2"); ok {
		t.Fatal("a person with nothing left must go")
	}
	if pe, ok := e.Get("p_1"); !ok || len(pe.Affiliations) != 0 || len(pe.Activity) != 1 {
		t.Fatalf("p_1 after drop = %+v", pe)
	}
	_ = e.SetSource("p_3", EnrichSourceGitdm, []EnrichedAffiliation{{Org: "Initech"}}, nil, enrichAt)
	if n, err := e.Prune(func(id string) bool { return id == "p_1" }); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	if err := e.Delete("p_1"); err != nil {
		t.Fatal(err)
	}
	if ids := e.IDs(); len(ids) != 0 {
		t.Fatalf("left over: %v", ids)
	}
	var nilStore *EnrichmentStore
	if err := nilStore.Delete("p_1"); err != nil {
		t.Fatal("Delete on an unwired store is a no-op")
	}
}

// A damaged file is set aside: enrichment can always be fetched again.
func TestEnrichmentStoreDamagedFileStartsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "customers")
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, EnrichmentFileName), []byte("{not json"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	e, err := OpenEnrichment(dir)
	if err != nil || len(e.IDs()) != 0 {
		t.Fatalf("damaged file = %v, %v", e.IDs(), err)
	}
}

func TestSuggestAffiliationWithGitdm(t *testing.T) {
	now := enrichAt
	gitdm := []EnrichedAffiliation{
		{Org: "Acme", From: "2019-01-01", Until: "2021-12-31", Source: EnrichSourceGitdm},
		{Org: "Globex", From: "2022-01-01", Source: EnrichSourceGitdm},
	}
	s := SuggestAffiliationWith(affPerson(ProfileField{}), gitdm, now)
	if s == nil || s.Value != "Globex" || s.Rule != AffiliationFromGitdm || s.Source != EnrichSourceGitdm || s.From != "2022-01-01" ||
		!strings.Contains(s.Evidence, "cncf/gitdm") {
		t.Fatalf("gitdm suggestion = %+v", s)
	}
	// gitdm outranks the GitHub company text.
	if s := SuggestAffiliationWith(affPerson(ProfileField{Value: "acme", Source: SourceAuto, Rule: ProfileSourceGitHub}), gitdm, now); s == nil || s.Rule != AffiliationFromGitdm {
		t.Fatalf("gitdm must outrank the company rule: %+v", s)
	}
	// Already shown: fall back to the offline rules.
	if s := SuggestAffiliationWith(affPerson(ProfileField{Value: "globex", Source: SourceAuto, Rule: ProfileSourceGitHub}), gitdm, now); s == nil || s.Rule != AffiliationFromCompany || s.Value != "Globex" {
		t.Fatalf("shown gitdm value = %+v, want the company tidy rule", s)
	}
	if s := SuggestAffiliationWith(affPerson(ProfileField{Value: "Globex", Source: SourceAuto, Rule: ProfileSourceGitHub}), gitdm, now); s != nil {
		t.Fatalf("nothing to suggest = %+v", s)
	}
	// Manual and hub always win.
	for _, src := range []Source{SourceManual, SourceHub} {
		if s := SuggestAffiliationWith(affPerson(ProfileField{Value: "Initech", Source: src}), gitdm, now); s != nil {
			t.Fatalf("%s organization challenged by %+v", src, s)
		}
	}
	// A dismissed value is not offered again.
	p := affPerson(ProfileField{})
	p.Profile.DismissedAffiliations = []string{"globex"}
	if s := SuggestAffiliationWith(p, gitdm, now); s != nil {
		t.Fatalf("dismissed gitdm value offered: %+v", s)
	}
	// Only past affiliations: nothing current to offer.
	if s := SuggestAffiliationWith(affPerson(ProfileField{}), gitdm[:1], now); s != nil {
		t.Fatalf("an ended affiliation was offered: %+v", s)
	}
	// Other sources do not feed this rule, and no enrichment is the old rule.
	other := []EnrichedAffiliation{{Org: "Umbrella", Source: EnrichSourceCrowdDev}}
	if s := SuggestAffiliationWith(affPerson(ProfileField{}), other, now); s != nil {
		t.Fatalf("crowd.dev must not feed the gitdm rule: %+v", s)
	}
	base := affPerson(ProfileField{Value: "acme", Source: SourceAuto, Rule: ProfileSourceGitHub})
	a, b := SuggestAffiliationWith(base, nil, now), SuggestAffiliation(base)
	if aj, _ := json.Marshal(a); string(aj) != mustJSON(t, b) {
		t.Fatalf("no enrichment changed the suggestion: %s vs %s", aj, mustJSON(t, b))
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The service offers a gitdm suggestion only while the source is switched
// on, never stores it, and Forget erases the person's enrichment.
func TestServiceEnrichmentSuggestionAndForget(t *testing.T) {
	s := newService(t)
	e, err := OpenEnrichment(filepath.Dir(s.Store().Path()))
	if err != nil {
		t.Fatal(err)
	}
	on := false
	s.SetEnrichment(e, func(source string) bool { return on && source == EnrichSourceGitdm })
	ada, err := s.Add(ownerV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetSource(ada.ID, EnrichSourceGitdm, []EnrichedAffiliation{{Org: "Globex", From: "2022-01-01"}}, nil, enrichAt); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ownerV, ada.ID); got.Affiliation != nil {
		t.Fatalf("source off, suggestion shown: %+v", got.Affiliation)
	}
	on = true
	got, err := s.Get(ownerV, ada.ID)
	if err != nil || got.Affiliation == nil || got.Affiliation.Source != EnrichSourceGitdm || got.Affiliation.Value != "Globex" {
		t.Fatalf("source on = %+v, %v", got.Affiliation, err)
	}
	if stored, _ := s.Store().Get(ada.ID); stored.Profile.Organization.Value != "" {
		t.Fatalf("a suggestion wrote the organization: %+v", stored.Profile.Organization)
	}
	if _, err := s.Forget(ownerV, ada.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Get(ada.ID); ok {
		t.Fatal("Forget left the person's enrichment behind")
	}
	if !s.Store().Forgotten(KindGitHub, "ada") {
		t.Fatal("Forget left no tombstone")
	}
}

// Public activity shows in the detail view only while its source is on.
func TestServicePublicContributionsFollowTheSwitch(t *testing.T) {
	s := newService(t)
	e, err := OpenEnrichment(filepath.Dir(s.Store().Path()))
	if err != nil {
		t.Fatal(err)
	}
	on := false
	s.SetEnrichment(e, func(source string) bool { return on && source == EnrichSourceDevstats })
	ada, err := s.Add(ownerV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetSource(ada.ID, EnrichSourceDevstats, nil, []EnrichedActivity{{Project: "All CNCF", Contributions: 42, URL: "https://all.devstats.cncf.io/d/9"}}, enrichAt); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ownerV, ada.ID); len(got.PublicContributions) != 0 {
		t.Fatalf("source off, activity shown: %+v", got.PublicContributions)
	}
	on = true
	got, err := s.Get(ownerV, ada.ID)
	if err != nil || len(got.PublicContributions) != 1 || got.PublicContributions[0].Contributions != 42 {
		t.Fatalf("source on = %+v, %v", got.PublicContributions, err)
	}
	if list, _ := s.List(ownerV); len(list.People) > 0 && len(list.People[0].PublicContributions) != 0 {
		t.Fatal("the list view must not carry public activity")
	}
}
