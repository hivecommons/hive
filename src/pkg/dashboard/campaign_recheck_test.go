package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agentparse"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/planning"
)

// writeCampaignArchiveFixture persists an archive exactly where the inception
// engine reads it, so tests can shape fields (State, Recheck) that the
// external-campaign API never sets.
func writeCampaignArchiveFixture(t *testing.T, dataDir string, archive knowledge.InceptionCampaignArchive) {
	t.Helper()
	dir := filepath.Join(dataDir, "inception", "campaigns", archive.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func shippedCampaignArchive(id string, now time.Time, recheck *knowledge.CampaignRecheck) knowledge.InceptionCampaignArchive {
	started := now.Add(-48 * time.Hour)
	return knowledge.InceptionCampaignArchive{
		ID: id, Title: "Shipped idea", Engine: "Spec Kit", Type: "inception",
		Repos:      []string{"acme/tool"},
		ArchivedAt: now.Add(-24 * time.Hour),
		Recheck:    recheck,
		State: &knowledge.InceptionState{
			Phase: knowledge.PhaseComplete, IdeaText: "Ship it", IdeaSlug: id,
			StartedAt: started, Answers: map[string]string{}, Questions: []knowledge.Question{},
		},
	}
}

func campaignRecheckAuditActions(s *Server) []string {
	var out []string
	for _, e := range s.audit.Recent(0) {
		if strings.HasPrefix(e.Action, "campaign_recheck") {
			out = append(out, e.Action)
		}
	}
	return out
}

func TestCampaignRecheckPureHelpers(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
		ok   bool
	}{
		{"acme/tool#42", "acme/tool", true},
		{"acme/tool!spec-1", "acme/tool", true},
		{"#42", "", false},
		{"!spec", "", false},
		{"plain-key", "", false},
	} {
		got, ok := splitRunRepo(tc.key)
		if got != tc.want || ok != tc.ok {
			t.Errorf("splitRunRepo(%q) = %q,%v want %q,%v", tc.key, got, ok, tc.want, tc.ok)
		}
	}

	if got := firstCampaignRepo(Campaign{Repos: []string{"  acme/one ", "acme/two"}}); got != "acme/one" {
		t.Errorf("firstCampaignRepo repos = %q", got)
	}
	if got := firstCampaignRepo(Campaign{Repos: []string{"  "}, RunKey: "acme/run!key"}); got != "acme/run" {
		t.Errorf("firstCampaignRepo blank repo should fall back to run key, got %q", got)
	}
	if got := firstCampaignRepo(Campaign{RunKey: "no-repo-key"}); got != "" {
		t.Errorf("firstCampaignRepo unparseable run key = %q", got)
	}
	if got := firstCampaignRepo(Campaign{}); got != "" {
		t.Errorf("firstCampaignRepo empty = %q", got)
	}

	if got := priorCampaignHead(Campaign{}); got != "" {
		t.Errorf("priorCampaignHead without drift = %q", got)
	}
	if got := priorCampaignHead(Campaign{Drift: &CampaignDrift{CurrentHeadSHA: "abc123"}}); got != "abc123" {
		t.Errorf("priorCampaignHead = %q", got)
	}

	if got := recheckLastDelta(Campaign{}); got != 0 {
		t.Errorf("recheckLastDelta empty = %d", got)
	}
	if got := recheckLastDelta(Campaign{Drift: &CampaignDrift{DeltaCount: 4}}); got != 4 {
		t.Errorf("recheckLastDelta drift = %d", got)
	}
	if got := recheckLastDelta(Campaign{Recheck: &CampaignRecheck{LastDeltaCount: 7}, Drift: &CampaignDrift{DeltaCount: 4}}); got != 7 {
		t.Errorf("recheckLastDelta should prefer recheck meta, got %d", got)
	}

	if !parseCampaignTime("  ").IsZero() || !parseCampaignTime("yesterday").IsZero() {
		t.Error("parseCampaignTime accepted invalid input")
	}
	if got := parseCampaignTime("2026-10-05T00:00:00Z"); !got.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("parseCampaignTime = %v", got)
	}

	// Task identity is whitespace- and case-insensitive so a re-wrapped plan
	// line is not reimported as a new delta.
	a := taskContentHash(agentparse.Task{Title: "Add   Helpers\tfor encoding"})
	b := taskHashFromTitle("add helpers for ENCODING")
	if a != b || len(a) != 64 {
		t.Errorf("task hashes differ: %s vs %s", a, b)
	}
	if taskHashFromTitle("add helpers") == taskHashFromTitle("add helper") {
		t.Error("distinct titles collided")
	}
}

func TestCampaignRecheckProjectionsFromArchive(t *testing.T) {
	basis := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	if got := campaignRecheckFromKnowledge(nil, true, basis); got != nil {
		t.Fatalf("nil recheck projected: %+v", got)
	}
	// Enabled with a zero basis and no LastAt has no schedule.
	got := campaignRecheckFromKnowledge(&knowledge.CampaignRecheck{Enabled: true, Interval: time.Hour}, false, time.Time{})
	if got == nil || got.NextAt != "" || got.Interval != "1h0m0s" || got.LastAt != "" {
		t.Fatalf("zero-basis recheck = %+v", got)
	}
	// Disabled rechecks never schedule, even with an interval.
	got = campaignRecheckFromKnowledge(&knowledge.CampaignRecheck{Enabled: false, Interval: time.Hour}, true, basis)
	if got == nil || got.NextAt != "" || !got.InFlight || got.Enabled {
		t.Fatalf("disabled recheck = %+v", got)
	}
	// LastAt wins over the archive basis.
	last := basis.Add(6 * time.Hour)
	got = campaignRecheckFromKnowledge(&knowledge.CampaignRecheck{Enabled: true, Interval: time.Hour, LastAt: last, LastDeltaCount: 3}, false, basis)
	if got == nil || got.NextAt != last.Add(time.Hour).Format(time.RFC3339) || got.LastAt != formatRunTime(last) || got.LastDeltaCount != 3 {
		t.Fatalf("recheck with last_at = %+v", got)
	}

	// Legacy archives carry only RecheckInterval; it still projects as enabled.
	legacy := campaignRecheckFromArchive(knowledge.InceptionCampaignArchive{RecheckInterval: 2 * time.Hour, ArchivedAt: basis}, false)
	if legacy == nil || !legacy.Enabled || legacy.Interval != "2h0m0s" || legacy.NextAt != basis.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("legacy interval projection = %+v", legacy)
	}
	if got := campaignRecheckFromArchive(knowledge.InceptionCampaignArchive{ArchivedAt: basis}, false); got != nil {
		t.Fatalf("archive without recheck projected: %+v", got)
	}

	if got := campaignDriftFromArchive(knowledge.InceptionCampaignArchive{}); got != nil {
		t.Fatalf("nil drift projected: %+v", got)
	}
	published := basis.Add(time.Minute)
	drift := campaignDriftFromArchive(knowledge.InceptionCampaignArchive{Drift: &knowledge.CampaignDrift{
		CodebaseChanged: true, PriorHeadSHA: "aaa", CurrentHeadSHA: "bbb", PriorRevision: "base", DeltaCount: 2, RecheckReason: recheckReasonCadence,
		External:      []knowledge.CampaignExternalEvidence{{Source: "upstream", Kind: "release", Title: "v2", URL: "https://example.invalid/v2", PublishedAt: published, Summary: "adds a thing"}},
		ExternalCount: 1,
		SourcesFailed: []knowledge.CampaignSourceFailure{{Name: "feed", Reason: "timeout"}},
	}})
	if drift == nil || !drift.CodebaseChanged || drift.PriorHeadSHA != "aaa" || drift.CurrentHeadSHA != "bbb" || drift.PriorRevision != "base" || drift.DeltaCount != 2 || drift.RecheckReason != recheckReasonCadence || drift.ExternalCount != 1 {
		t.Fatalf("drift projection = %+v", drift)
	}
	if len(drift.External) != 1 || drift.External[0].Source != "upstream" || drift.External[0].Kind != "release" || drift.External[0].Title != "v2" || drift.External[0].URL != "https://example.invalid/v2" || drift.External[0].PublishedAt != formatRunTime(published) || drift.External[0].Summary != "adds a thing" {
		t.Fatalf("external evidence projection = %+v", drift.External)
	}
	if len(drift.SourcesFailed) != 1 || drift.SourcesFailed[0].Name != "feed" || drift.SourcesFailed[0].Reason != "timeout" {
		t.Fatalf("source failures projection = %+v", drift.SourcesFailed)
	}
	if campaignExternalEvidenceFromKnowledge(nil) != nil || campaignSourceFailuresFromKnowledge(nil) != nil {
		t.Fatal("empty knowledge slices should project as nil")
	}
}

func TestDecorateCampaignRechecksFillsDefaultsAndSchedule(t *testing.T) {
	var nilServer *Server
	nilServer.decorateCampaignRechecks(map[string]Campaign{"x": {}}) // nil receiver is a no-op

	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	s.deps.Config.Runs.Spektacular.Recheck.Enabled = true
	s.deps.Config.Runs.Spektacular.Recheck.DefaultInterval = 3 * time.Hour
	lastActivity := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	lastAt := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	campaigns := map[string]Campaign{
		// No recheck meta: inherits the default cadence and schedules from LastActivity.
		"base": {ID: "base", LastActivity: formatRunTime(lastActivity), CurrentStage: "completed"},
		// Explicit LastAt beats LastActivity.
		"explicit": {ID: "explicit", LastActivity: formatRunTime(lastActivity), Recheck: &CampaignRecheck{Enabled: true, Interval: "2h", LastAt: formatRunTime(lastAt)}},
		// No activity at all schedules from "now".
		"fresh": {ID: "fresh", Recheck: &CampaignRecheck{Enabled: true, Interval: "1h"}},
		// Invalid interval leaves NextAt empty.
		"broken": {ID: "broken", Recheck: &CampaignRecheck{Enabled: true, Interval: "soon"}},
		// Disabled rechecks are left unscheduled.
		"off": {ID: "off", Recheck: &CampaignRecheck{Enabled: false, Interval: "1h"}},
		// A live revision of "base" marks the base in flight.
		"base-rev-2": {ID: "base-rev-2", RevisionOf: "base", CurrentStage: StageSpec, Status: "parked", Recheck: &CampaignRecheck{Enabled: true, Interval: "1h", NextAt: "keep"}},
		// A completed revision does not.
		"explicit-rev-2": {ID: "explicit-rev-2", RevisionOf: "explicit", CurrentStage: "completed", Status: "shipped", Recheck: &CampaignRecheck{Enabled: false}},
	}
	before := time.Now()
	s.decorateCampaignRechecks(campaigns)

	base := campaigns["base"]
	if base.Recheck == nil || !base.Recheck.Enabled || base.Recheck.Interval != "3h0m0s" || !base.Recheck.InFlight {
		t.Fatalf("base recheck defaults = %+v", base.Recheck)
	}
	if base.Recheck.NextAt != lastActivity.Add(3*time.Hour).Format(time.RFC3339) {
		t.Fatalf("base next_at = %q", base.Recheck.NextAt)
	}
	explicit := campaigns["explicit"]
	if explicit.Recheck.InFlight || explicit.Recheck.NextAt != lastAt.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("explicit recheck = %+v", explicit.Recheck)
	}
	fresh := campaigns["fresh"]
	next, err := time.Parse(time.RFC3339, fresh.Recheck.NextAt)
	if err != nil || next.Before(before.Add(time.Hour).Truncate(time.Second)) {
		t.Fatalf("fresh next_at = %q (err %v)", fresh.Recheck.NextAt, err)
	}
	if campaigns["broken"].Recheck.NextAt != "" || campaigns["off"].Recheck.NextAt != "" {
		t.Fatalf("unschedulable rechecks got a next_at: broken=%+v off=%+v", campaigns["broken"].Recheck, campaigns["off"].Recheck)
	}
	if campaigns["base-rev-2"].Recheck.NextAt != "keep" {
		t.Fatal("existing next_at was overwritten")
	}

	// A disabled default leaves campaigns without meta opted out.
	s.deps.Config.Runs.Spektacular.Recheck.Enabled = false
	opted := map[string]Campaign{"quiet": {ID: "quiet"}}
	s.decorateCampaignRechecks(opted)
	if r := opted["quiet"].Recheck; r == nil || r.Enabled || r.NextAt != "" {
		t.Fatalf("default-off recheck = %+v", r)
	}
}

func TestRecheckIntervalPrecedence(t *testing.T) {
	var nilServer *Server
	if got := nilServer.recheckInterval(Campaign{}); got != config.DefaultSpektacularRecheckInterval {
		t.Fatalf("nil server interval = %v", got)
	}
	if got := nilServer.recheckInterval(Campaign{Recheck: &CampaignRecheck{Interval: "45m"}}); got != 45*time.Minute {
		t.Fatalf("campaign interval = %v", got)
	}
	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	s.deps.Config.Runs.Spektacular.Recheck.DefaultInterval = 5 * time.Hour
	if got := s.recheckInterval(Campaign{Recheck: &CampaignRecheck{Interval: "-1h"}}); got != 5*time.Hour {
		t.Fatalf("non-positive campaign interval should fall back to config, got %v", got)
	}
	if got := s.recheckInterval(Campaign{Recheck: &CampaignRecheck{Interval: "later"}}); got != 5*time.Hour {
		t.Fatalf("invalid campaign interval should fall back to config, got %v", got)
	}
}

func TestCampaignRecheckHandlerRejections(t *testing.T) {
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular = config.SpektacularConfig{Enabled: true}

	// Owner gate first: an anonymous caller never reaches the campaign store.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/campaigns/anything/recheck?force=true", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous recheck = %d body=%s, want 403", rec.Code, rec.Body.String())
	}

	// A blank path value (after unescaping) is a 400, not a lookup.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/campaigns/%20/recheck", nil)
	req.SetPathValue("id", "%20")
	markOwnerRequest(req)
	s.handleCampaignRecheck(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "campaign id required") {
		t.Fatalf("blank id recheck = %d body=%s, want 400", rec.Code, rec.Body.String())
	}

	// No inception engine: the store is unavailable and surfaces as a 500.
	s.deps.Inception = nil
	rec = doOwnerPostAsUser(s, "/api/campaigns/stable-spec-8665/recheck?force=true", "bob", map[string]string{})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "campaign store unavailable") {
		t.Fatalf("storeless recheck = %d body=%s, want 500", rec.Code, rec.Body.String())
	}

	// Unknown campaign is a 404 distinct from "disabled".
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	rec = doOwnerPostAsUser(s, "/api/campaigns/no-such-campaign/recheck?force=true", "bob", map[string]string{})
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "campaign not found") {
		t.Fatalf("unknown recheck = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func TestTriggerCampaignRecheckRequiresStore(t *testing.T) {
	var nilServer *Server
	if _, err := nilServer.triggerCampaignRecheck(context.Background(), "x", "owner", recheckReasonManual, true, time.Time{}); err == nil {
		t.Fatal("nil server accepted a recheck")
	}
	s := &Server{deps: &Dependencies{Config: &config.Config{}, Inception: knowledge.NewInceptionEngine(t.TempDir(), nil, slog.Default())}}
	if _, err := s.triggerCampaignRecheck(context.Background(), "x", "owner", recheckReasonManual, true, time.Time{}); err == nil || !strings.Contains(err.Error(), "campaign store unavailable") {
		t.Fatalf("hub-less recheck err = %v", err)
	}
}

func TestTriggerCampaignRecheckByRunKeyAndMetadata(t *testing.T) {
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular = config.SpektacularConfig{Enabled: true}
	deps.Config.Runs.Spektacular.Recheck.DefaultInterval = 9 * time.Hour
	dataDir := t.TempDir()
	s.deps.Inception = knowledge.NewInceptionEngine(dataDir, nil, s.logger)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	base, err := s.deps.Inception.UpsertExternalCampaign("run-base", "Research", "acme/tool!run-base", "Spektacular", "spektacular", nil, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.deps.Inception.SetCampaignDrift(base.ID, &knowledge.CampaignDrift{CurrentHeadSHA: "deadbeef", DeltaCount: 5}); err != nil {
		t.Fatal(err)
	}
	// The campaign's own cadence (4h) must beat the configured default (9h),
	// and its last delta count must carry forward onto the base metadata.
	if _, err := s.deps.Inception.SetCampaignRecheck(base.ID, &knowledge.CampaignRecheck{Enabled: false, Interval: 4 * time.Hour, LastDeltaCount: 5}); err != nil {
		t.Fatal(err)
	}

	// Lookup by RunKey (the archive's Source) rather than ID, with a zero
	// "now" that the trigger must fill in itself.
	revision, err := s.triggerCampaignRecheck(context.Background(), "acme/tool!run-base", "bob", recheckReasonManual, true, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if revision.RevisionOf != base.ID || revision.Revision != 2 || revision.CurrentStage != StageSpec {
		t.Fatalf("revision = %+v", revision)
	}
	if revision.Recheck == nil || !revision.Recheck.Enabled || revision.Recheck.Interval != "4h0m0s" {
		t.Fatalf("revision recheck meta = %+v", revision.Recheck)
	}
	if revision.Drift == nil || revision.Drift.PriorHeadSHA != "deadbeef" || revision.Drift.PriorRevision != base.ID || revision.Drift.RecheckReason != recheckReasonManual {
		t.Fatalf("revision drift = %+v", revision.Drift)
	}
	// Repo derived from the run key prefixes the stage lease key.
	if _, held := s.contributeHub.runLeaseHolder("acme/tool!"+revision.ID+":"+StageSpec, time.Now()); !held {
		t.Fatalf("stage lease for revision %s not recorded", revision.ID)
	}

	stored, err := s.deps.Inception.LoadCampaignArchive(base.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Recheck == nil || !stored.Recheck.Enabled || stored.Recheck.LastAt.IsZero() || stored.Recheck.LastDeltaCount != 5 || stored.Recheck.Interval != 4*time.Hour {
		t.Fatalf("base recheck meta after trigger = %+v", stored.Recheck)
	}
	if stored.Drift == nil || stored.Drift.CurrentHeadSHA != "deadbeef" {
		t.Fatalf("base drift was rewritten: %+v", stored.Drift)
	}
	if actions := campaignRecheckAuditActions(s); len(actions) != 1 || actions[0] != "campaign_recheck" {
		t.Fatalf("audit actions = %v", actions)
	}

	// A second manual trigger finds the live revision and refuses.
	if _, err := s.triggerCampaignRecheck(context.Background(), base.ID, "bob", recheckReasonManual, true, now); err != errRecheckInFlight {
		t.Fatalf("in-flight trigger err = %v", err)
	}
}

func TestTriggerCampaignRecheckSurfacesStoreWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory modes do not stop root; cannot inject a write failure")
	}
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular = config.SpektacularConfig{Enabled: true}
	dataDir := t.TempDir()
	s.deps.Inception = knowledge.NewInceptionEngine(dataDir, nil, s.logger)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if _, err := s.deps.Inception.UpsertExternalCampaign("sealed", "Research", "sealed", "Spektacular", "spektacular", nil, now); err != nil {
		t.Fatal(err)
	}
	campaignDir := filepath.Join(dataDir, "inception", "campaigns", "sealed")
	if err := os.Chmod(campaignDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(campaignDir, 0o700) })

	if _, err := s.triggerCampaignRecheck(context.Background(), "sealed", "bob", recheckReasonManual, true, now); err == nil {
		t.Fatal("unwritable archive accepted a recheck")
	}
	archives, err := s.deps.Inception.ListCampaignArchives()
	if err != nil || len(archives) != 1 {
		t.Fatalf("failed trigger left archives = %+v (err %v)", archives, err)
	}
	if actions := campaignRecheckAuditActions(s); len(actions) != 0 {
		t.Fatalf("failed trigger was audited: %v", actions)
	}
}

func TestTickCampaignRechecksCadence(t *testing.T) {
	var nilServer *Server
	nilServer.TickCampaignRechecks(context.Background(), time.Now()) // nil receiver is a no-op

	s, deps := runsTestServer(t)
	dataDir := t.TempDir()
	s.deps.Inception = knowledge.NewInceptionEngine(dataDir, nil, s.logger)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	due := &knowledge.CampaignRecheck{Enabled: true, Interval: time.Hour, LastAt: now.Add(-2 * time.Hour)}
	writeCampaignArchiveFixture(t, dataDir, shippedCampaignArchive("shipped-due", now, due))
	writeCampaignArchiveFixture(t, dataDir, shippedCampaignArchive("shipped-later", now, &knowledge.CampaignRecheck{Enabled: true, Interval: time.Hour, LastAt: now}))
	writeCampaignArchiveFixture(t, dataDir, shippedCampaignArchive("shipped-opted-out", now, &knowledge.CampaignRecheck{Enabled: false, Interval: time.Hour, LastAt: now.Add(-2 * time.Hour)}))
	inProgress := shippedCampaignArchive("still-drafting", now, due)
	inProgress.State.Phase = knowledge.PhaseClarify
	writeCampaignArchiveFixture(t, dataDir, inProgress)
	if _, err := s.deps.Inception.UpsertExternalCampaign("external-plan", "Research", "acme/tool!plan", "Spektacular", "spektacular", []string{"acme/tool"}, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	countArchives := func() int {
		t.Helper()
		archives, err := s.deps.Inception.ListCampaignArchives()
		if err != nil {
			t.Fatal(err)
		}
		return len(archives)
	}

	// Cadence is scheduler-owned: with the feature off nothing moves.
	deps.Config.Runs.Spektacular.Recheck.Enabled = false
	s.TickCampaignRechecks(context.Background(), now)
	if got := countArchives(); got != 5 {
		t.Fatalf("disabled tick changed archives: %d", got)
	}

	deps.Config.Runs.Spektacular.Recheck.Enabled = true
	s.TickCampaignRechecks(context.Background(), now)
	if got := countArchives(); got != 6 {
		t.Fatalf("archives after due tick = %d, want exactly one new revision", got)
	}
	revision, err := s.deps.Inception.LoadCampaignArchive("shipped-due-rev-2")
	if err != nil {
		t.Fatalf("due campaign was not revised: %v", err)
	}
	if revision.RevisionOf != "shipped-due" || revision.Drift == nil || revision.Drift.RecheckReason != recheckReasonCadence || revision.Drift.PriorRevision != "shipped-due" {
		t.Fatalf("cadence revision = %+v drift=%+v", revision, revision.Drift)
	}
	if revision.Recheck == nil || !revision.Recheck.Enabled || revision.Recheck.Interval != time.Hour {
		t.Fatalf("cadence revision inherits interval: %+v", revision.Recheck)
	}
	base, err := s.deps.Inception.LoadCampaignArchive("shipped-due")
	if err != nil {
		t.Fatal(err)
	}
	if base.Recheck == nil || !base.Recheck.LastAt.Equal(now) {
		t.Fatalf("base last_at not advanced: %+v", base.Recheck)
	}
	entries := s.audit.Recent(0)
	var cadenceAudit int
	for _, e := range entries {
		if e.Action == "campaign_recheck" && e.User == "system" && strings.Contains(e.Detail, "shipped-due") {
			cadenceAudit++
		}
	}
	if cadenceAudit != 1 {
		t.Fatalf("cadence audit entries = %d in %+v", cadenceAudit, entries)
	}

	// Same instant again: the live revision marks the base in flight and the
	// revision itself is not completed, so neither is rechecked.
	s.TickCampaignRechecks(context.Background(), now)
	if got := countArchives(); got != 6 {
		t.Fatalf("repeat tick created archives: %d", got)
	}
	// Advancing past the next cadence still waits on the in-flight revision.
	s.TickCampaignRechecks(context.Background(), now.Add(3*time.Hour))
	if got := countArchives(); got != 6 {
		t.Fatalf("in-flight base was rechecked again: %d", got)
	}
}

func TestTickCampaignRechecksLogsTriggerFailure(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	cfg.Runs.Spektacular.Recheck.Enabled = true
	s := &Server{deps: &Dependencies{Config: cfg, Inception: knowledge.NewInceptionEngine(dataDir, nil, logger)}, logger: logger}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	writeCampaignArchiveFixture(t, dataDir, shippedCampaignArchive("shipped-due", now, &knowledge.CampaignRecheck{Enabled: true, Interval: time.Hour, LastAt: now.Add(-2 * time.Hour)}))

	// Without a contribute hub the trigger cannot record the stage lease; the
	// tick must swallow and log the error rather than panic or loop.
	s.TickCampaignRechecks(context.Background(), now)
	archives, err := s.deps.Inception.ListCampaignArchives()
	if err != nil || len(archives) != 1 {
		t.Fatalf("failed cadence trigger left archives = %+v (err %v)", archives, err)
	}

	// An unreadable campaign root makes the listing fail; the tick returns quietly.
	if os.Geteuid() != 0 {
		root := filepath.Join(dataDir, "inception", "campaigns")
		if err := os.Chmod(root, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
		s.TickCampaignRechecks(context.Background(), now)
	}
}

func TestImportRunPlanRecheckDeltaOnlyImportsNewTasks(t *testing.T) {
	_, s, store, _ := spekHub(t)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	// Plans for a run that is not a recheck revision take the direct path.
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] Add helpers [agent_suitable]\n2. [T2] Write docs [agent_suitable]"); err != nil {
		t.Fatal(err)
	}
	_, baseEpic := s.findRunEpic(spekRunKey)
	if baseEpic == nil {
		t.Fatal("base epic missing")
	}
	// A retained campaign that is not a revision also takes the direct path.
	if _, err := s.deps.Inception.UpsertExternalCampaign(spekRunKey, "Research", spekRunKey, "Spektacular", "spektacular", []string{spekRepo}, now); err != nil {
		t.Fatal(err)
	}
	if handled, err := s.importRecheckDelta(spekRunKey, spekRepo, "1. [T9] Anything", runPlanSource); handled || err != nil {
		t.Fatalf("non-revision archive handled=%v err=%v", handled, err)
	}

	revision, err := s.deps.Inception.ReviseExternalCampaign(spekRunKey, "Research", spekRunKey, "Spektacular", "spektacular", "owner", []string{spekRepo}, now)
	if err != nil {
		t.Fatal(err)
	}
	// A revision without drift evidence is not a recheck either.
	if handled, _ := s.importRecheckDelta(revision.ID, spekRepo, "1. [T9] Anything", runPlanSource); handled {
		t.Fatal("revision without drift was treated as a recheck")
	}
	if _, err := s.deps.Inception.SetCampaignDrift(revision.ID, &knowledge.CampaignDrift{RecheckReason: recheckReasonManual}); err != nil {
		t.Fatal(err)
	}

	plan := strings.Join([]string{
		"1. [T1] Add   helpers [agent_suitable]", // same task, re-wrapped: not a delta
		"2. [T3] Migrate storage (depends: T1) [agent_suitable]",
		"3. [T4] Get legal sign-off (human_required)",
	}, "\n")
	if err := s.ImportRunPlan(revision.ID, spekRepo, plan); err != nil {
		t.Fatal(err)
	}
	_, revEpic := s.findRunEpic(revision.ID)
	if revEpic == nil {
		t.Fatal("revision epic missing")
	}
	tree, err := planning.GetPlanTree(store, revEpic.ID)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, child := range tree.Children {
		titles = append(titles, child.Title)
	}
	if len(titles) != 2 || strings.Contains(strings.Join(titles, "|"), "helpers") {
		t.Fatalf("delta plan children = %v, want only the two new tasks", titles)
	}
	stored, err := s.deps.Inception.LoadCampaignArchive(revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Drift == nil || stored.Drift.DeltaCount != 2 || stored.Drift.RecheckReason != recheckReasonManual {
		t.Fatalf("delta count not recorded: %+v", stored.Drift)
	}
	baseTree, err := planning.GetPlanTree(store, baseEpic.ID)
	if err != nil || len(baseTree.Children) != 2 {
		t.Fatalf("base plan changed: %+v err=%v", baseTree, err)
	}
	if actions := campaignRecheckAuditActions(s); len(actions) != 0 {
		t.Fatalf("plain delta import was audited: %v", actions)
	}
}

func TestImportRunPlanRecheckDeltaEmptyAndCapped(t *testing.T) {
	_, s, store, _ := spekHub(t)
	s.deps.Inception = knowledge.NewInceptionEngine(t.TempDir(), nil, s.logger)
	s.deps.Config.Runs.Spektacular.Recheck.MaxDeltaTasks = 1
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] Add helpers [agent_suitable]"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deps.Inception.UpsertExternalCampaign(spekRunKey, "Research", spekRunKey, "Spektacular", "spektacular", []string{spekRepo}, now); err != nil {
		t.Fatal(err)
	}
	newRevision := func() string {
		t.Helper()
		revision, err := s.deps.Inception.ReviseExternalCampaign(spekRunKey, "Research", spekRunKey, "Spektacular", "spektacular", "owner", []string{spekRepo}, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.deps.Inception.SetCampaignDrift(revision.ID, &knowledge.CampaignDrift{}); err != nil {
			t.Fatal(err)
		}
		return revision.ID
	}

	// Nothing new: no epic is minted for the revision and the empty delta is audited.
	empty := newRevision()
	if err := s.ImportRunPlan(empty, spekRepo, "1. [T1] add helpers [agent_suitable]"); err != nil {
		t.Fatal(err)
	}
	if _, epic := s.findRunEpic(empty); epic != nil {
		t.Fatalf("empty delta minted an epic: %+v", epic)
	}
	stored, err := s.deps.Inception.LoadCampaignArchive(empty)
	if err != nil || stored.Drift == nil || stored.Drift.DeltaCount != 0 {
		t.Fatalf("empty delta drift = %+v err=%v", stored.Drift, err)
	}
	// Not a task list at all is also empty.
	if err := s.ImportRunPlan(empty, spekRepo, "prose without tasks"); err != nil {
		t.Fatal(err)
	}

	// Three new tasks against a cap of one: only the first is imported, the
	// recorded delta is the cap, and the truncation is audited.
	capped := newRevision()
	if err := s.ImportRunPlan(capped, spekRepo, "1. [T2] Second\n2. [T3] Third\n3. [T4] Fourth"); err != nil {
		t.Fatal(err)
	}
	_, epic := s.findRunEpic(capped)
	if epic == nil {
		t.Fatal("capped delta minted no epic")
	}
	tree, err := planning.GetPlanTree(store, epic.ID)
	if err != nil || len(tree.Children) != 1 || tree.Children[0].Title != "Second" {
		t.Fatalf("capped plan tree = %+v err=%v", tree, err)
	}
	stored, err = s.deps.Inception.LoadCampaignArchive(capped)
	if err != nil || stored.Drift == nil || stored.Drift.DeltaCount != 1 {
		t.Fatalf("capped delta drift = %+v err=%v", stored.Drift, err)
	}
	// Recent is newest-first.
	actions := campaignRecheckAuditActions(s)
	if len(actions) != 3 || actions[0] != "campaign_recheck_delta_capped" || actions[1] != "campaign_recheck_delta_empty" || actions[2] != "campaign_recheck_delta_empty" {
		t.Fatalf("audit actions = %v", actions)
	}
}

func TestDeltaTaskListAndPriorHashes(t *testing.T) {
	var nilServer *Server
	if got := nilServer.priorTaskHashes("any"); len(got) != 0 {
		t.Fatalf("nil server hashes = %v", got)
	}
	if out, delta, capped := nilServer.deltaTaskList("prior", "1. [T1] Solo task"); delta != 1 || capped || !strings.Contains(out, "1. [T1] Solo task") {
		t.Fatalf("nil server delta = %q,%d,%v", out, delta, capped)
	}
	if _, delta, capped := nilServer.deltaTaskList("prior", "no tasks here"); delta != 0 || capped {
		t.Fatalf("no-task delta = %d,%v", delta, capped)
	}

	_, s, store, _ := spekHub(t)
	// A nil store entry is skipped; the real store holds the prior plan.
	s.deps.BeadStores["empty"] = nil
	// An unrelated epic (different run key) must not contribute hashes.
	other, err := store.Create("Other run", beads.TypeEpic, beads.PriorityMedium, planning.ArchitectAgentName, "other-run")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(other.ID, planning.MetaRunKey, "other-run"); err != nil {
		t.Fatal(err)
	}
	other, _ = store.Get(other.ID)
	if _, err := planning.DecomposeFromOutput(store, other, "1. [T1] Unrelated task", planning.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] Add helpers\n2. [T2] Write docs"); err != nil {
		t.Fatal(err)
	}
	seen := s.priorTaskHashes(spekRunKey)
	if len(seen) != 2 || !seen[taskHashFromTitle("Add helpers")] || !seen[taskHashFromTitle("write DOCS")] || seen[taskHashFromTitle("Unrelated task")] {
		t.Fatalf("prior hashes = %v", seen)
	}
	if got := s.priorTaskHashes("missing-run"); len(got) != 0 {
		t.Fatalf("unknown run hashes = %v", got)
	}

	plan := "1. [T1] Add helpers\n2. Migrate storage (depends: T1, T2) [agent_suitable]\n3. [T4] Legal (human_required)"
	out, delta, capped := s.deltaTaskList(spekRunKey, plan)
	if delta != 2 || capped {
		t.Fatalf("delta = %d capped=%v out=%q", delta, capped, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("delta list = %q", out)
	}
	// Renumbered from 1; a missing ref is synthesised; depends and execution tags survive.
	if lines[0] != "1. [T1] Migrate storage (depends: T1, T2) [agent_suitable]" {
		t.Fatalf("first delta line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "2. [T4] Legal") || !strings.HasSuffix(lines[1], "[human_required]") {
		t.Fatalf("second delta line = %q", lines[1])
	}

	s.deps.Config.Runs.Spektacular.Recheck.MaxDeltaTasks = 1
	out, delta, capped = s.deltaTaskList(spekRunKey, plan)
	if delta != 1 || !capped || strings.Count(out, "\n") != 1 {
		t.Fatalf("capped delta = %d capped=%v out=%q", delta, capped, out)
	}
}

func TestCurrentGitHeadAndEvidence(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Point git at a repository that does not exist so the probe fails even
	// when the temp directory happens to live inside a checkout.
	t.Setenv("GIT_DIR", filepath.Join(dir, "no-such-repo"))
	// Outside a repository the head is unknown and never reported as drift.
	if head := currentGitHead(nil); head != "" {
		t.Fatalf("head outside repo = %q", head)
	}
	evidence, err := (codebaseHeadEvidenceSource{}).Evidence(context.Background(), Campaign{}, "prior")
	if err != nil || evidence.CodebaseChanged || evidence.PriorHeadSHA != "prior" || evidence.CurrentHeadSHA != "" {
		t.Fatalf("evidence outside repo = %+v err=%v", evidence, err)
	}
	// A cancelled context short-circuits the git call.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if head := currentGitHead(ctx); head != "" {
		t.Fatalf("head with cancelled ctx = %q", head)
	}
}
