package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	convergenceaudit "github.com/hivecommons/hive/pkg/convergence/audit"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/convergence/publish"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
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

func TestHandleRunAuditIndexCrossRepoJoin(t *testing.T) {
	s, deps := runsTestServer(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	epic, err := store.Create("audit index plan", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusApproved
		b.Metadata[planning.MetaIssueRepo] = "myorg/repo1"
		b.Metadata[planning.MetaIssueNumber] = "8617"
	}); err != nil {
		t.Fatal(err)
	}
	deps.BeadStores = map[string]*beads.Store{"architect": store}
	now := time.Now().UTC()
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: "myorg/repo1#8617",
		Kind:     timeline.KindStageApproval,
		Agent:    "owner",
		At:       now.Add(-2 * time.Minute).UnixMilli(),
		Attrs:    map[string]string{"stage": StagePlan, "gen": "3"},
	})
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: "myorg/repo2#42",
		Kind:     timeline.KindStageReceipt,
		Agent:    "runner",
		At:       now.Add(-1 * time.Minute).UnixMilli(),
		Attrs:    map[string]string{"receipt_digest": "sha256:abc", "gen": "1"},
	})
	s.audit.Log("owner", "lease_stage_advanced", "run=myorg/repo1#8617, repo=myorg/repo1, stage=implement, gen=3", "runner")

	rec := doOwnerGet(s, "/api/runs/audit?repo=myorg/repo1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/runs/audit = %d body=%s", rec.Code, rec.Body.String())
	}
	var got runAuditIndexResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range got.Items {
		if item.Repo != "myorg/repo1" || item.Run != "myorg/repo1#8617" {
			t.Fatalf("unexpected item outside requested repo/run: %+v", item)
		}
		seen[item.Source+":"+item.Kind] = true
	}
	for _, key := range []string{"timeline:stage_approval", "audit_log:lease_stage_advanced", "plan_epic:plan_epic"} {
		if !seen[key] {
			t.Fatalf("missing %s in %+v", key, got.Items)
		}
	}
}

func TestHandleRunAuditIndexCanonicalRunIncludesLeaseShapedReceipts(t *testing.T) {
	s, _ := runsTestServer(t)
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: "myorg/repo1!myorg/repo1#8617:plan",
		Kind:     timeline.KindStageReceipt,
		Agent:    "runner",
		At:       time.Now().UTC().UnixMilli(),
		Attrs:    map[string]string{"receipt_digest": "sha256:lease"},
	})
	rec := doOwnerGet(s, "/api/runs/audit?run=myorg/repo1%238617&kind=stage_receipt")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET lease receipt = %d body=%s", rec.Code, rec.Body.String())
	}
	var got runAuditIndexResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v, want one canonical receipt", got.Items)
	}
	item := got.Items[0]
	if item.Run != "myorg/repo1#8617" || item.Source != "lease_receipt" || item.Attrs["receipt_digest"] != "sha256:lease" {
		t.Fatalf("item = %+v", item)
	}
}

func TestHandleRunAuditIndexPagination(t *testing.T) {
	s, _ := runsTestServer(t)
	now := time.Now().UTC()
	for i, kind := range []timeline.Kind{timeline.KindStageReceipt, timeline.KindStageApproval, timeline.KindStageCompleted} {
		s.LifecycleTimeline().Record(timeline.Event{
			IssueRef: "myorg/repo1#8617",
			Kind:     kind,
			Agent:    "runner",
			At:       now.Add(time.Duration(i) * time.Minute).UnixMilli(),
		})
	}
	rec := doOwnerGet(s, "/api/runs/audit?run=myorg/repo1%238617&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET first page = %d body=%s", rec.Code, rec.Body.String())
	}
	var first runAuditIndexResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextPageToken == "" {
		t.Fatalf("first page = %+v, want 2 items and next token", first)
	}
	rec = doOwnerGet(s, "/api/runs/audit?run=myorg/repo1%238617&limit=2&page_token="+first.NextPageToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET second page = %d body=%s", rec.Code, rec.Body.String())
	}
	var second runAuditIndexResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.NextPageToken != "" {
		t.Fatalf("second page = %+v, want final single item", second)
	}
}

func TestHandleRunAuditIndexRetentionExpiryMarker(t *testing.T) {
	s, _ := runsTestServer(t)
	since := time.Now().UTC().AddDate(0, 0, -auditMaxAgeDays-1).Format(time.RFC3339)
	rec := doOwnerGet(s, "/api/runs/audit?run=myorg/repo1%238617&kind=expired&since="+url.QueryEscape(since))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET expired = %d body=%s", rec.Code, rec.Body.String())
	}
	var got runAuditIndexResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) == 0 {
		t.Fatalf("got no expired marker: %+v", got)
	}
	for _, item := range got.Items {
		if item.Kind != "expired" || item.Status != "expired" || item.Run != "myorg/repo1#8617" || item.Attrs["state"] != "Unknown" {
			t.Fatalf("bad expired marker: %+v", item)
		}
	}
	if got.Retention.ExpiredMarkerKind != "expired" || got.Retention.AuditLog == "" || got.Retention.Timeline == "" {
		t.Fatalf("retention contract missing: %+v", got.Retention)
	}
}
