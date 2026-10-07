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

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
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
		// A rewound generation (Drift set, not completed) is in flight (v6).
		"rewound": {ID: "rewound", CurrentStage: StageSpec, Status: "parked", Drift: &CampaignDrift{RecheckReason: recheckReasonManual}, Recheck: &CampaignRecheck{Enabled: true, Interval: "1h", NextAt: "keep"}},
		// A completed generation is not, even with Drift recorded.
		"settled": {ID: "settled", CurrentStage: "completed", Status: "shipped", Drift: &CampaignDrift{RecheckReason: recheckReasonCadence}, Recheck: &CampaignRecheck{Enabled: false}},
	}
	before := time.Now()
	s.decorateCampaignRechecks(campaigns)

	base := campaigns["base"]
	if base.Recheck == nil || !base.Recheck.Enabled || base.Recheck.Interval != "3h0m0s" || base.Recheck.InFlight {
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
	if r := campaigns["rewound"].Recheck; r.NextAt != "keep" || !r.InFlight {
		t.Fatalf("rewound recheck = %+v, want in flight with next_at kept", r)
	}
	if campaigns["settled"].Recheck.InFlight {
		t.Fatal("completed generation reported in flight")
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

func TestTickCampaignRechecksLogsTriggerFailure(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	cfg.Runs.Spektacular.Recheck.Enabled = true
	s := &Server{deps: &Dependencies{Config: cfg, Inception: knowledge.NewInceptionEngine(dataDir, nil, logger)}, logger: logger}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	writeCampaignArchiveFixture(t, dataDir, shippedCampaignArchive("shipped-due", now, &knowledge.CampaignRecheck{Enabled: true, Interval: time.Hour, LastAt: now.Add(-2 * time.Hour)}))

	// Without a contribute hub the trigger cannot admit the spec stage; the
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

func TestCurrentGitHeadAndEvidence(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Point git at a repository that does not exist so the probe fails even
	// when the temp directory happens to live inside a checkout.
	t.Setenv("GIT_DIR", filepath.Join(dir, "no-such-repo"))
	// Outside a repository the head is unknown and never reported as drift.
	//nolint:staticcheck // SA1012: the nil-ctx fallback branch is the subject under test.
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
