package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

func orchestrationServer(t *testing.T, mode string) *Server {
	t.Helper()
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{IntegrationOrchestration: config.IntegrationOrchestrationConfig{
		Mode: mode, Scopes: []config.IntegrationScope{{Repo: "o/r", Base: "v5"}}}}}
	s.integration.path = filepath.Join(t.TempDir(), "integration.jsonl")
	return s
}

func TestIntegrationBothDispatchLanesAndOffIdentity(t *testing.T) {
	raw := &ghpkg.ActionableResult{Issues: ghpkg.IssueResultFromItems([]ghpkg.Issue{{Repo: "o/r", Number: 1}, {Repo: "elsewhere/r", Number: 2}})}
	for _, mode := range []string{config.IntegrationOff, config.IntegrationAdvisory, config.IntegrationEnforce} {
		t.Run(mode, func(t *testing.T) {
			s := orchestrationServer(t, mode)
			projected := s.IntegrationKickAdmission(raw)
			if mode == config.IntegrationOff && projected != raw {
				t.Fatal("OFF changed existing pointer/behavior")
			}
			want := 2
			if mode == config.IntegrationEnforce {
				want = 1
			}
			if projected.Issues.Count != want || len(raw.Issues.Items) != 2 {
				t.Fatalf("internal dispatch mode=%s got=%+v", mode, projected.Issues)
			}
			h := &ContributeWSHub{server: s}
			ref := worksource.Ref{SourceType: "linear", Repo: "o/r", ExternalID: "ENG-1"}
			decision := h.evaluateContributorNeutralAdmission(nil, contributorAdmissionCandidate{repoFull: "o/r", ref: ref})
			if decision.admitted != (mode != config.IntegrationEnforce) {
				t.Fatalf("contributor mode=%s got=%+v", mode, decision)
			}
			if mode != config.IntegrationOff {
				store, err := s.integrationStore()
				if err != nil {
					t.Fatal(err)
				}
				if len(store.Snapshot(time.Now()).Intents) != 2 {
					t.Fatal("actual dispatch did not persist canonical intents")
				}
			}
		})
	}
}

func TestIntegrationAPIRollbackAndUnknownTelemetry(t *testing.T) {
	s := orchestrationServer(t, config.IntegrationAdvisory)
	read := httptest.NewRecorder()
	s.handleIntegrationGet(read, convergenceGetReq(true))
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), "unknown") || !strings.Contains(read.Body.String(), "runtime_commit") {
		t.Fatalf("%d %s", read.Code, read.Body)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/config/integration-orchestration", strings.NewReader(`{"mode":"enforce"}`))
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	w := httptest.NewRecorder()
	s.handleIntegrationConfigPut(w, req)
	if w.Code != http.StatusBadRequest || s.integrationConfig().Mode != config.IntegrationAdvisory {
		t.Fatal("invalid configuration mutated active mode")
	}
	body, _ := json.Marshal(config.IntegrationOrchestrationConfig{Mode: config.IntegrationOff})
	req = httptest.NewRequest(http.MethodPut, "/api/config/integration-orchestration", bytes.NewReader(body))
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	w = httptest.NewRecorder()
	s.handleIntegrationConfigPut(w, req)
	if w.Code != http.StatusOK || s.integrationConfig().Mode != config.IntegrationOff {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestIntegrationWebhookReplayJobCostAndMissingAttempt(t *testing.T) {
	s := orchestrationServer(t, config.IntegrationAdvisory)
	now := time.Now().UTC()
	body := []byte(`{"repository":{"full_name":"o/r"},"workflow_job":{"id":10,"run_id":20,"run_attempt":1,"head_sha":"` + strings.Repeat("a", 40) + `","head_branch":"v5","runner_id":30,"status":"completed","conclusion":"cancelled","started_at":"2026-10-30T20:00:00Z","completed_at":"2026-10-30T20:10:00Z"}}`)
	if err := s.observeIntegrationWebhook("workflow_job", "delivery", body, now); err != nil {
		t.Fatal(err)
	}
	if err := s.observeIntegrationWebhook("workflow_job", "delivery", body, now.Add(time.Hour)); err != nil {
		t.Fatal("delivery replay changed observation", err)
	}
	store, err := s.integrationStore()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Conclusion != "cancelled" {
		t.Fatalf("%+v", snapshot)
	}
	bad := bytes.Replace(body, []byte(`"run_attempt":1`), []byte(`"run_attempt":0`), 1)
	if s.observeIntegrationWebhook("workflow_job", "missing-attempt", bad, now) == nil {
		t.Fatal("missing attempt silently attributed")
	}
}

func TestIntegrationLeaseAtomicAdmissionAndExpiry(t *testing.T) {
	s := orchestrationServer(t, config.IntegrationEnforce)
	now := time.Now().UTC()
	store, err := s.integrationStore()
	if err != nil {
		t.Fatal(err)
	}
	i := integration.Intent{Version: 1, Key: "o/r#1", Repo: "o/r", Base: "v5", Generation: 1, Plan: "unresolved", PlanNode: "o/r#1", ScopeKnown: true, Provenance: "dispatch-enumerator", ObservedAt: now}
	if err := store.EnsureIntent(i); err != nil {
		t.Fatal(err)
	}
	c := integration.Coverage{Repo: "o/r", Base: "v5", From: now.Add(-2 * time.Hour), Through: now, Complete: true, Source: "github-actions"}
	if err := store.Append(integration.Event{Version: 1, ID: "coverage", At: now, Coverage: &c}); err != nil {
		t.Fatal(err)
	}
	if err := s.integrationLease("o/r#1", "o/r", "alice", 1, now.Add(time.Hour), now, false); err != nil {
		t.Fatal(err)
	}
	if err := s.integrationLease("o/r#1", "o/r", "bob", 1, now.Add(time.Hour), now, false); err == nil {
		t.Fatal("competing lease acquired active reservation")
	}
	if len(store.Snapshot(now.Add(16*time.Minute)).Reservations) != 0 {
		t.Fatal("orphaned reservation did not expire")
	}
	s.integrationRelease("o/r#1", "contributor/alice", 1, "test")
	if len(store.Snapshot(now).Reservations) != 0 {
		t.Fatal("release retained reservation")
	}
}

func TestIntegrationScopeAmbiguityAndUnavailableLedger(t *testing.T) {
	s := orchestrationServer(t, config.IntegrationEnforce)
	s.deps.Config.IntegrationOrchestration.Scopes = append(s.deps.Config.IntegrationOrchestration.Scopes, config.IntegrationScope{Repo: "o/r", Base: "other"})
	d := s.integrationDispatchDecision(worksource.Ref{Repo: "o/r", Number: 1})
	if d.Allowed || !strings.Contains(d.Reason, "base unknown") {
		t.Fatalf("%+v", d)
	}
}

func TestIntegrationRealClaimCommitSerializesCapacity(t *testing.T) {
	_, server, ledger := claimsHub(t)
	server.deps.Config.IntegrationOrchestration = config.IntegrationOrchestrationConfig{Mode: config.IntegrationEnforce, MaxActive: 1, Scopes: []config.IntegrationScope{{Repo: "myorg/repo1", Base: "v5"}}}
	server.integration.path = filepath.Join(t.TempDir(), "journal")
	store, err := server.integrationStore()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, number := range []int{1, 2} {
		intent := integration.Intent{Version: 1, Key: fmt.Sprintf("myorg/repo1#%d", number), Repo: "myorg/repo1", Base: "v5", Generation: 1, Plan: "unresolved", PlanNode: "owner-enrolled", ScopeKnown: true, Provenance: "owner", ObservedAt: now}
		if err := store.EnsureIntent(intent); err != nil {
			t.Fatal(err)
		}
	}
	coverage := integration.Coverage{Repo: "myorg/repo1", Base: "v5", From: now.Add(-2 * time.Hour), Through: now, Complete: true, Source: "github-actions"}
	if err := store.Append(integration.Event{Version: 1, ID: "coverage", At: now, Coverage: &coverage}); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	changed := make(chan bool, 2)
	for _, number := range []int{1, 2} {
		workers.Add(1)
		go func(number int) {
			defer workers.Done()
			result, err := ledger.Claim(claims.Request{Repo: "myorg/repo1", Issue: number, Holder: "agent", Kind: claims.KindAgent})
			if err != nil {
				t.Error(err)
			}
			changed <- result.Outcome == claims.OutcomeClaimed
		}(number)
	}
	workers.Wait()
	close(changed)
	count := 0
	for granted := range changed {
		if granted {
			count++
		}
	}
	if count != 1 || len(store.Snapshot(now).Reservations) != 1 {
		t.Fatalf("claims=%d reservations=%d", count, len(store.Snapshot(now).Reservations))
	}
}

func TestIntegrationActualContributorSelectionModes(t *testing.T) {
	for _, mode := range []string{config.IntegrationOff, config.IntegrationAdvisory, config.IntegrationEnforce} {
		t.Run(mode, func(t *testing.T) {
			hub, server, _ := claimsHub(t)
			server.deps.Config.IntegrationOrchestration = config.IntegrationOrchestrationConfig{Mode: mode, Scopes: []config.IntegrationScope{{Repo: "myorg/repo1", Base: "v5"}}}
			server.integration.path = filepath.Join(t.TempDir(), "journal")
			setStatusIssues(server, intgIssue(1, "independent work", "someone", nil))
			connection := &ContributorConnection{profile: &ContributorProfile{GitHubUsername: "worker", ContributorID: "c-worker", TrustTier: "contributor"}, lastPong: time.Now()}
			hub.mu.Lock()
			hub.connections["conn-worker"] = connection
			hub.mu.Unlock()
			message := hub.selectTask(connection)
			admitted := message != nil && message.Type == "task_assign"
			if admitted != (mode != config.IntegrationEnforce) {
				t.Fatalf("mode=%s message=%+v", mode, message)
			}
		})
	}
}

func TestIntegrationFailedLedgerCannotPreventDisabling(t *testing.T) {
	server := orchestrationServer(t, config.IntegrationEnforce)
	if err := os.WriteFile(server.integration.path, []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := server.integrationStore(); err == nil {
		t.Fatal("invalid journal accepted")
	}
	request := httptest.NewRequest(http.MethodPut, "/api/config/integration-orchestration", strings.NewReader(`{"mode":"off"}`))
	request.Header.Set("X-Hive-Role", "owner")
	request.Header.Set(ownerRoleVerifiedHeader, "true")
	response := httptest.NewRecorder()
	server.handleIntegrationConfigPut(response, request)
	if response.Code != http.StatusOK || server.integrationConfig().Mode != config.IntegrationOff {
		t.Fatalf("%d %s", response.Code, response.Body)
	}
}
