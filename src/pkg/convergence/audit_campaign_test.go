package convergence_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/audit"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/outputschema"
)

type fakeGitHubClient struct{ calls int }

func (f *fakeGitHubClient) Count() int { return f.calls }

type fakeSoak struct {
	mode       string
	generation uint64
	records    int
}

func (f *fakeSoak) RecordAuditSoak(mode string, generation uint64) {
	f.mode, f.generation, f.records = mode, generation, f.records+1
}

func TestAuditCampaignFixtureReportOnlyShadowSoak(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	now := time.Date(2026, 9, 22, 18, 0, 0, 0, time.UTC)
	store, ledger, journal, proofs := auditStores(t)
	gh := &fakeGitHubClient{}
	soak := &fakeSoak{}
	res, err := audit.Run(audit.Options{CampaignKey: "fixture", ScopeDir: "testdata/audit-scope", Store: store, Ledger: ledger, Journal: journal, ProofStore: proofs, Mode: config.ConvergenceModeShadow, Generation: 5, Holder: "tester", Now: func() time.Time { return now }, GitHub: gh, Soak: soak, CrashAfterBeginComponent: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	if gh.Count() != 0 {
		t.Fatalf("audit pilot made %d GitHub calls, want zero", gh.Count())
	}
	if soak.records != 1 || soak.mode != config.ConvergenceModeShadow || soak.generation != 5 {
		t.Fatalf("soak record = (%s,%d)x%d, want (shadow,5)x1", soak.mode, soak.generation, soak.records)
	}
	if res.Burndown.SatisfiedObligations != nil || res.Burndown.NullReason == "" {
		t.Fatalf("unknown inspection must report null burndown count with reason: %+v", res.Burndown)
	}
	if len(res.Burndown.UnknownEvidence) != 1 || res.Burndown.UnknownEvidence[0] != "billing" {
		t.Fatalf("unknown evidence = %v", res.Burndown.UnknownEvidence)
	}
	assertFindingStates(t, store, 2, 1)
	assertStageReceiptsValidate(t, res.Receipts)
	if got := len(proofs.List()); got != 5 {
		t.Fatalf("proof receipts after one unknown = %d, want 5", got)
	}

	res, err = audit.Run(audit.Options{CampaignKey: "fixture", ScopeDir: "testdata/audit-scope", Store: store, Ledger: ledger, Journal: journal, ProofStore: proofs, Mode: config.ConvergenceModeShadow, Generation: 5, Holder: "tester", Now: func() time.Time { return now.Add(2 * time.Hour) }, GitHub: gh, Soak: soak})
	if err != nil {
		t.Fatal(err)
	}
	if res.Burndown.SatisfiedObligations == nil || *res.Burndown.SatisfiedObligations != 6 {
		t.Fatalf("rerun burndown = %+v, want full scope completion", res.Burndown)
	}
	assertFindingStates(t, store, 2, 1)
	assertPublicationNone(t, store)
	if got := len(proofs.List()); got != 6 {
		t.Fatalf("proof receipts after reconcile = %d, want 6", got)
	}
}

func TestAuditCampaignReconcilesCrashAfterFindingWithoutDuplicate(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	now := time.Date(2026, 9, 22, 19, 0, 0, 0, time.UTC)
	store, ledger, journal, proofs := auditStores(t)
	_, err := audit.Run(audit.Options{
		CampaignKey: "fixture", ScopeDir: "testdata/audit-scope", Store: store, Ledger: ledger, Journal: journal,
		ProofStore: proofs, Mode: config.ConvergenceModeShadow, Generation: 5, Holder: "tester", Now: func() time.Time { return now },
		AfterRecordFindings: func(component string) error {
			if component == "config" {
				return errors.New("simulated crash before RecordResult")
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("simulated crash should surface an uncertain effect")
	}
	assertFindingStates(t, store, 2, 0)

	_, err = audit.Run(audit.Options{
		CampaignKey: "fixture", ScopeDir: "testdata/audit-scope", Store: store, Ledger: ledger, Journal: journal,
		ProofStore: proofs, Mode: config.ConvergenceModeShadow, Generation: 5, Holder: "tester", Now: func() time.Time { return now.Add(2 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFindingStates(t, store, 2, 1)
}

func TestAuditCampaignRefusesGitHubCredentials(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "secret")
	store, ledger, journal, _ := auditStores(t)
	_, err := audit.Run(audit.Options{CampaignKey: "fixture", ScopeDir: "testdata/audit-scope", Store: store, Ledger: ledger, Journal: journal, Mode: config.ConvergenceModeShadow, Generation: 1})
	if err == nil {
		t.Fatal("audit campaign must refuse to run with GitHub credentials in the environment")
	}
}

func auditStores(t *testing.T) (*beads.Store, *mutation.Ledger, *mutation.Journal, *proof.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := beads.NewStore(filepath.Join(dir, "beads"))
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := mutation.OpenLedger(filepath.Join(dir, "ledger.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	proofs, err := proof.Open(filepath.Join(dir, "proofs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store, ledger, journal, proofs
}

func assertFindingStates(t *testing.T, store *beads.Store, wantValidated, wantDuplicate int) {
	t.Helper()
	validated, duplicate := 0, 0
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta("audit_kind") != "finding" {
			continue
		}
		switch b.Meta("audit_finding_state") {
		case "validated":
			validated++
		case "duplicate_of":
			if b.Meta("duplicate_of") == "" {
				t.Fatalf("duplicate finding %s lacks duplicate_of", b.ID)
			}
			duplicate++
		}
	}
	if validated != wantValidated || duplicate != wantDuplicate {
		t.Fatalf("finding states validated=%d duplicate=%d, want %d/%d", validated, duplicate, wantValidated, wantDuplicate)
	}
}

func assertPublicationNone(t *testing.T, store *beads.Store) {
	t.Helper()
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta("audit_kind") == "publication" && b.Meta("publication_state") == "none" {
			return
		}
	}
	t.Fatal("publication bead with publication_state=none not found")
}

func assertStageReceiptsValidate(t *testing.T, receipts []outputschema.StageReceipt) {
	t.Helper()
	if len(receipts) != 5 {
		t.Fatalf("receipts = %d, want 5 because one component is Unknown", len(receipts))
	}
	for _, receipt := range receipts {
		raw, err := json.Marshal(outputschema.AgentReport{Lane: "audit", Kind: outputschema.KindStageReceipt, Findings: []outputschema.Finding{}, PRsOpened: []outputschema.PROpened{}, BeadsFiled: []outputschema.BeadFiled{}, Summary: "audit receipt", Receipt: &receipt})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := outputschema.Validate(raw); err != nil {
			t.Fatalf("receipt for %s failed outputschema validation: %v\n%s", receipt.WorkKey, err, raw)
		}
	}
}
