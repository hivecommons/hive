package main

import (
	"testing"
	"time"

	"github.com/hivecommons/colony/resolver"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
)

// highLoginMatch is gitdm's answer for a login match (confidence high).
func highLoginMatch(org string, from time.Time) resolver.Affiliation {
	return resolver.Affiliation{Org: org, From: from, Source: "gitdm", Confidence: resolver.ConfidenceHigh, MatchedBy: "github_login"}
}

func autoAcceptHarness(t *testing.T, policy string) *enrichmentHarness {
	t.Helper()
	t.Setenv(config.EnvCustomersEnrichmentAutoAccept, "")
	h := newEnrichmentHarness(t, true)
	h.cfg.Customers.Enrichment.AutoAccept = policy
	since := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	h.fake.answers["ada"] = []resolver.Affiliation{highLoginMatch("Globex", since)}
	h.fake.answers["bea"] = []resolver.Affiliation{{Org: "Initech", From: since, Source: "gitdm",
		Confidence: resolver.ConfidenceMedium, MatchedBy: "email"}}
	return h
}

// With the default policy, off, a gitdm match stays a suggestion.
func TestCustomersAutoAcceptOffKeepsSuggestion(t *testing.T) {
	h := autoAcceptHarness(t, "")
	ada, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if res := h.pass(); res.AutoAccepted != 0 {
		t.Fatalf("off policy accepted %d", res.AutoAccepted)
	}
	got, _ := h.svc.svc.Get(ownerViewer, ada.ID)
	if got.Profile.Organization.Value != "" || got.Affiliation == nil || got.Affiliation.Value != "Globex" {
		t.Fatalf("organization %+v, suggestion %+v", got.Profile.Organization, got.Affiliation)
	}
}

// gitdm_high fills an empty organization from a login match, labelled and
// dismissible; an email match is never accepted.
func TestCustomersAutoAcceptGitdmHigh(t *testing.T) {
	h := autoAcceptHarness(t, config.CustomersAutoAcceptGitdmHigh)
	ada, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	bea, err := h.svc.svc.Add(ownerViewer, "Bea", []customers.Identity{{Kind: customers.KindGitHub, Value: "bea"}})
	if err != nil {
		t.Fatal(err)
	}
	res := h.pass()
	if res.AutoAccepted != 1 {
		t.Fatalf("pass = %+v, want one auto-accept", res)
	}
	got, _ := h.svc.svc.Get(ownerViewer, ada.ID)
	org := got.Profile.Organization
	if org.Value != "Globex" || org.Source != customers.SourceAuto || org.Rule != customers.AffiliationAutoAcceptedGitdm {
		t.Fatalf("organization = %+v", org)
	}
	if b, _ := h.svc.svc.Get(ownerViewer, bea.ID); b.Profile.Organization.Value != "" {
		t.Fatalf("an email match was auto-accepted: %+v", b.Profile.Organization)
	}
	found := false
	for _, a := range h.audit {
		if a == ada.ID+"|"+customersAutoAcceptAuditProvider {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit = %v, want the auto-accept line", h.audit)
	}
	// Dismissing clears it and it is not accepted again.
	if _, err := h.svc.svc.Edit(ownerViewer, ada.ID, customers.Edit{Op: customers.OpAffiliationDismiss, Value: "Globex"}); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(48 * time.Hour)
	h.fake.loaded = time.Time{}
	if res := h.pass(); res.AutoAccepted != 0 {
		t.Fatalf("a dismissed value was accepted again: %+v", res)
	}
	if got, _ := h.svc.svc.Get(ownerViewer, ada.ID); got.Profile.Organization.Value != "" {
		t.Fatalf("dismiss left %+v", got.Profile.Organization)
	}
}

// gitdm_high never overwrites a value a person set or one shared from the
// hub.
func TestCustomersAutoAcceptNeverOverwritesUserOrHub(t *testing.T) {
	h := autoAcceptHarness(t, config.CustomersAutoAcceptGitdmHigh)
	ada, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.svc.Edit(ownerViewer, ada.ID, customers.Edit{Op: customers.OpProfile, Field: customers.FieldOrganization, Value: "Initech"}); err != nil {
		t.Fatal(err)
	}
	if res := h.pass(); res.AutoAccepted != 0 {
		t.Fatalf("overwrote a user value: %+v", res)
	}
	if got, _ := h.svc.svc.Get(ownerViewer, ada.ID); got.Profile.Organization.Value != "Initech" {
		t.Fatalf("organization = %+v", got.Profile.Organization)
	}
	store := h.svc.svc.Store()
	if _, err := store.ResetProfileField(ada.ID, customers.FieldOrganization); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ApplyHubMatch(ada.ID, "ada", customers.HubMatchEntry{Organization: "Umbrella"}); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(48 * time.Hour)
	h.fake.loaded = time.Time{}
	if res := h.pass(); res.AutoAccepted != 0 {
		t.Fatalf("overwrote a hub value: %+v", res)
	}
	if got, _ := h.svc.svc.Get(ownerViewer, ada.ID); got.Profile.Organization.Value != "Umbrella" || got.Profile.Organization.Source != customers.SourceHub {
		t.Fatalf("organization = %+v", got.Profile.Organization)
	}
}
