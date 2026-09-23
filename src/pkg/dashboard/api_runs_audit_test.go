package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	convergenceaudit "github.com/hivecommons/hive/pkg/convergence/audit"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/convergence/publish"
)

type fakeAuditPublisher struct{ calls int }

func (f *fakeAuditPublisher) PublishCampaign(context.Context, publish.Campaign, []publish.Finding, publish.Grant, *outcome.Ledger) (publish.CampaignResult, error) {
	f.calls++
	return publish.CampaignResult{Publications: map[string]publish.Publication{
		"one": {State: publish.StateWithheld, Reason: publish.ReasonMode},
	}}, nil
}

func TestHandleRunAuditRequiresOwner(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/audit", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("POST /api/runs/audit without verified owner = %d, want 403", rec.Code)
	}
}

func TestHandleRunAuditRunsWithPublicationDisabled(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s, deps := apiServer(t)
	scopeDir := writeAuditScope(t)
	store, ledger, journal := auditHandlerState(t)
	pub := &fakeAuditPublisher{}
	deps.Config.Publication.Enabled = false
	deps.BeadStores = map[string]*beads.Store{"audit": store}
	deps.AuditLedger = ledger
	deps.AuditJournal = journal
	deps.AuditPublisher = pub

	rec := doOwnerPost(s, "/api/runs/audit", runAuditRequest{
		CampaignKey: "handler-disabled",
		ScopeDir:    scopeDir,
		Generation:  7,
	})
	if rec.Code != 200 {
		t.Fatalf("POST /api/runs/audit = %d body=%s", rec.Code, rec.Body.String())
	}
	if pub.calls != 0 {
		t.Fatalf("disabled publication called publisher %d times", pub.calls)
	}
	var got runAuditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || !got.PublicationSkipped || got.PublicationReason != "publication.disabled" || got.Findings != 1 || got.Receipts != 1 {
		t.Fatalf("response = %+v", got)
	}
}

func TestHandleRunAuditPublishesWhenEnabled(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s, deps := apiServer(t)
	scopeDir := writeAuditScope(t)
	store, ledger, journal := auditHandlerState(t)
	var pub *fakeAuditPublisher
	deps.Config.Publication.Enabled = true
	deps.Config.Convergence.Mode = config.ConvergenceModeShadow
	deps.BeadStores = map[string]*beads.Store{"scanner": store}
	deps.AuditLedger = ledger
	deps.AuditJournal = journal
	deps.AuditPublisherFunc = func() convergenceaudit.FindingPublisher { return pub }
	pub = &fakeAuditPublisher{}

	rec := doOwnerPost(s, "/api/runs/audit", runAuditRequest{
		CampaignKey: "handler-happy",
		ScopeDir:    scopeDir,
		Store:       "scanner",
	})
	if rec.Code != 200 {
		t.Fatalf("POST /api/runs/audit = %d body=%s", rec.Code, rec.Body.String())
	}
	if pub.calls != 1 {
		t.Fatalf("publisher calls = %d, want 1", pub.calls)
	}
	var got runAuditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PublicationSkipped || got.Publication == nil || got.Publication.Publications[publish.StateWithheld] != 1 {
		t.Fatalf("publication response = %+v", got)
	}
}

func writeAuditScope(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	scope := convergenceaudit.Scope{Components: []convergenceaudit.Component{{
		Name:    "api",
		Files:   []string{"api.go"},
		Content: "handler",
		Findings: []convergenceaudit.Finding{{
			Title: "wire audit campaign",
			Files: []string{"api.go"},
		}},
	}}}
	data, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "components.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func auditHandlerState(t *testing.T) (*beads.Store, *mutation.Ledger, *mutation.Journal) {
	t.Helper()
	dir := t.TempDir()
	store, err := beads.NewStore(filepath.Join(dir, "beads"))
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := mutation.OpenLedger(filepath.Join(dir, "claims.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store, ledger, journal
}
