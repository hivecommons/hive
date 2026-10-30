package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
)

type integrationRuntime struct {
	mu            sync.Mutex
	once          sync.Once
	store         *integration.Store
	err           error
	path          string
	lastReconcile time.Time
}

func (s *Server) IntegrationNativeAdmission(_ context.Context, repo string, pr *gh.PullRequest) (string, error) {
	cfg := s.integrationConfig()
	if pr == nil || !cfg.Allows(repo, pr.GetBase().GetRef()) {
		return "", nil
	}
	store, err := s.integrationStore()
	if err != nil {
		if cfg.Mode == config.IntegrationEnforce {
			return "workflow observations unavailable", err
		}
		return "", nil
	}
	d, err := store.Decide(cfg, integration.Input{Key: fmt.Sprintf("native/%s/%d", repo, pr.GetNumber()), Repo: repo, Base: pr.GetBase().GetRef(), Now: time.Now()})
	if err != nil && cfg.Mode == config.IntegrationEnforce {
		return "admission evaluation unavailable", err
	}
	if !d.Allowed {
		return d.Reason, nil
	}
	return "", nil
}

func (s *Server) integrationConfig() config.IntegrationOrchestrationConfig {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return config.IntegrationOrchestrationConfig{}.Effective()
	}
	s.integration.mu.Lock()
	defer s.integration.mu.Unlock()
	return s.deps.Config.IntegrationOrchestration.Effective()
}

func (s *Server) integrationStore() (*integration.Store, error) {
	s.integration.once.Do(func() {
		path := s.integration.path
		if path == "" {
			path = filepath.Join(filepath.Dir(taskLeasesFile), "integration-orchestration.jsonl")
		}
		s.integration.store, s.integration.err = integration.Open(path)
	})
	return s.integration.store, s.integration.err
}

// integrationPlan verifies the existing approved DAG on every use. Neither a
// text similarity hint nor an intent's provenance can approve its own edges.
func (s *Server) integrationPlan(i integration.Intent) (bool, map[string]integration.Dependency) {
	deps := map[string]integration.Dependency{}
	if s.deps == nil {
		return false, deps
	}
	tree, ok := planning.FindPlanTree(s.deps.BeadStores, i.Plan)
	if !ok || !tree.Approved || tree.DesignRevision != i.PlanRevision {
		return false, deps
	}
	var node *planning.PlanChild
	nodes := map[string]planning.PlanChild{}
	for n := range tree.Children {
		child := tree.Children[n]
		nodes[child.ID] = child
		if child.ID == i.PlanNode {
			node = &child
		}
	}
	if node == nil {
		return false, deps
	}
	// Published child beads retain canonical issue metadata. A missing binding
	// is unknown, not permission to attach someone else's approved plan.
	bound := false
	for _, store := range s.deps.BeadStores {
		if store == nil {
			continue
		}
		b, err := store.Get(i.PlanNode)
		if err == nil && b.Meta(planning.MetaIssueRepo)+"#"+b.Meta(planning.MetaIssueNumber) == i.Key {
			bound = true
			break
		}
	}
	if !bound {
		return false, deps
	}
	edges := map[string]bool{}
	for _, id := range node.DependsOn {
		edges[id] = true
	}
	for _, p := range i.Prerequisites {
		child, found := nodes[p.PlanNode]
		if !found || !edges[p.PlanNode] {
			continue
		}
		keyBound := false
		for _, store := range s.deps.BeadStores {
			if store == nil {
				continue
			}
			b, err := store.Get(p.PlanNode)
			if err == nil && b.Meta(planning.MetaIssueRepo)+"#"+b.Meta(planning.MetaIssueNumber) == p.Key {
				keyBound = true
				break
			}
		}
		if keyBound {
			deps[p.Key] = integration.Dependency{Known: true, Satisfied: child.Status == beads.StatusClosed || child.Status == beads.StatusDone}
		}
	}
	return true, deps
}

func (s *Server) integrationInput(key, repo, base string, now time.Time) integration.Input {
	in := integration.Input{Key: key, Repo: repo, Base: base, Now: now}
	store, err := s.integrationStore()
	if err != nil {
		return in
	}
	for _, i := range store.Snapshot(now).Intents {
		if i.Key == key {
			in.PlanApproved, in.Dependencies = s.integrationPlan(i)
			in.PlanRevision = i.PlanRevision
			break
		}
	}
	return in
}

// integrationDispatchDecision is shared by the internal and contributor lanes.
// The scope's base is explicit because issue enumerations do not carry a base.
// An ambiguous multi-base issue is not silently assigned to either cohort.
func (s *Server) integrationDispatchDecision(ref worksource.Ref) integration.Decision {
	cfg := s.integrationConfig()
	out := integration.Decision{Allowed: true}
	if cfg.Mode == config.IntegrationOff {
		return out
	}
	base := ""
	for _, scope := range cfg.Scopes {
		if !strings.EqualFold(scope.Repo, ref.Repo) {
			continue
		}
		if base != "" {
			out.Reason = "issue base unknown across multiple enrolled bases"
			out.WouldBlock = true
			out.Allowed = cfg.Mode != config.IntegrationEnforce
			return out
		}
		base = scope.Base
	}
	if base == "" {
		return out
	}
	store, err := s.integrationStore()
	if err != nil {
		out.Reason = "integration ledger unavailable: " + err.Error()
		out.WouldBlock, out.Allowed = true, cfg.Mode != config.IntegrationEnforce
		return out
	}
	if ref.Key() != "" {
		now := time.Now()
		intent := integration.Intent{Version: integration.SchemaVersion, Key: ref.Key(), Repo: ref.Repo, Base: base, Generation: 1,
			Plan: "unresolved", PlanNode: ref.Key(), ScopeKnown: false, Confidence: "unknown", Provenance: "dispatch-enumerator", ObservedAt: now}
		if err := store.EnsureIntent(intent); err != nil {
			return integration.Decision{Allowed: cfg.Mode != config.IntegrationEnforce, WouldBlock: true, Reason: "intent persistence unavailable: " + err.Error()}
		}
	}
	in := s.integrationInput(ref.Key(), ref.Repo, base, time.Now())
	out, err = store.Decide(cfg, in)
	if err != nil {
		out = integration.Decision{Allowed: cfg.Mode != config.IntegrationEnforce, WouldBlock: true, Reason: err.Error()}
	}
	return out
}

func (s *Server) integrationAgingRanks(keys []string) map[string]int {
	ranks := map[string]int{}
	if s.integrationConfig().Mode != config.IntegrationEnforce {
		return ranks
	}
	store, err := s.integrationStore()
	if err != nil {
		return ranks
	}
	for rank, key := range store.Order(keys) {
		ranks[key] = rank
	}
	return ranks
}

// IntegrationKickAdmission is independent of convergence.mode. OFF returns the
// original pointer; advisory never filters or changes scheduler ordering.
func (s *Server) IntegrationKickAdmission(raw *ghpkg.ActionableResult) *ghpkg.ActionableResult {
	if s == nil || raw == nil || s.integrationConfig().Mode == config.IntegrationOff {
		return raw
	}
	out := *raw
	items := make([]ghpkg.Issue, 0, len(raw.Issues.Items))
	for _, issue := range raw.Issues.Items {
		ref := worksource.Ref{Repo: issue.Repo, Number: issue.Number, SourceType: issue.SourceType, ExternalID: issue.ExternalID}
		d := s.integrationDispatchDecision(ref)
		if d.Allowed {
			items = append(items, issue)
		}
		if d.WouldBlock && s.logger != nil {
			s.logger.Info("integration authoring admission", "lane", "internal", "work", ref.Key(), "enforced", !d.Allowed, "reason", d.Reason)
		}
	}
	if s.integrationConfig().Mode != config.IntegrationEnforce {
		return raw
	}
	keys := make([]string, 0, len(items))
	for _, i := range items {
		keys = append(keys, worksource.Ref{Repo: i.Repo, Number: i.Number, SourceType: i.SourceType, ExternalID: i.ExternalID}.Key())
	}
	ranks := s.integrationAgingRanks(keys)
	buckets := map[string][]ghpkg.Issue{}
	for _, i := range items {
		buckets[i.Priority] = append(buckets[i.Priority], i)
	}
	for priority, bucket := range buckets {
		sort.SliceStable(bucket, func(a, b int) bool {
			ka := worksource.Ref{Repo: bucket[a].Repo, Number: bucket[a].Number, SourceType: bucket[a].SourceType, ExternalID: bucket[a].ExternalID}.Key()
			kb := worksource.Ref{Repo: bucket[b].Repo, Number: bucket[b].Number, SourceType: bucket[b].SourceType, ExternalID: bucket[b].ExternalID}.Key()
			return ranks[ka] < ranks[kb]
		})
		buckets[priority] = bucket
	}
	for n, i := range items {
		items[n] = buckets[i.Priority][0]
		buckets[i.Priority] = buckets[i.Priority][1:]
	}
	out.Issues = ghpkg.IssueResultFromItems(items)
	return &out
}

// IntegrationClaimBlockReason runs in the existing claims admission callback,
// before claiming. Existing ownership remains the sole dedup authority.
func (s *Server) IntegrationClaimBlockReason(repo string, number int) string {
	d := s.integrationDispatchDecision(worksource.Ref{Repo: repo, Number: number})
	if !d.Allowed {
		return d.Reason
	}
	return ""
}

func (s *Server) integrationClaimObserved(c claims.Claim) {
	if c.Kind == claims.KindContributor {
		return
	} // contributor reservations bind the server-issued lease below
	cfg := s.integrationConfig()
	if cfg.Mode != config.IntegrationEnforce {
		return
	}
	store, err := s.integrationStore()
	if err != nil {
		s.webhookLogger().Error("integration claim observation failed", "error", err)
		return
	}
	for _, scope := range cfg.Scopes {
		if !strings.EqualFold(scope.Repo, c.Repo) {
			continue
		}
		in := s.integrationInput(c.Key(), c.Repo, scope.Base, time.Now())
		in.Claim, in.ClaimGeneration, in.ClaimExpiresAt, in.Reserve = c.Holder, uint64(c.ClaimedAt.UnixNano()), c.ExpiresAt, true
		d, err := store.Decide(cfg, in)
		if err != nil || !d.Allowed {
			s.webhookLogger().Warn("integration reservation not acquired; existing claim retained", "key", c.Key(), "reason", d.Reason, "error", err)
		}
	}
}

func (s *Server) integrationClaimCommit(c claims.Claim) (string, error) {
	cfg := s.integrationConfig()
	if cfg.Mode != config.IntegrationEnforce || c.Kind == claims.KindContributor {
		return "", nil
	}
	for _, scope := range cfg.Scopes {
		if !strings.EqualFold(scope.Repo, c.Repo) {
			continue
		}
		store, err := s.integrationStore()
		if err != nil {
			return "integration ledger unavailable", err
		}
		in := s.integrationInput(c.Key(), c.Repo, scope.Base, time.Now())
		in.Claim, in.ClaimGeneration = c.Holder, uint64(c.ClaimedAt.UnixNano())
		in.ClaimExpiresAt, in.Reserve = c.ExpiresAt, true
		d, err := store.Decide(cfg, in)
		if err != nil || !d.Allowed {
			return d.Reason, err
		}
	}
	return "", nil
}

func (s *Server) integrationLease(key, repo, identity string, gen uint64, expires, now time.Time) error {
	cfg := s.integrationConfig()
	if cfg.Mode != config.IntegrationEnforce {
		return nil
	}
	for _, scope := range cfg.Scopes {
		if !strings.EqualFold(scope.Repo, repo) {
			continue
		}
		store, err := s.integrationStore()
		if err != nil {
			return err
		}
		in := s.integrationInput(key, repo, scope.Base, now)
		in.Reserve, in.Claim, in.ClaimGeneration, in.ClaimExpiresAt = true, "contributor/"+identity, gen, expires
		d, err := store.Decide(cfg, in)
		if err != nil {
			return err
		}
		if !d.Allowed {
			return fmt.Errorf("integration admission: %s", d.Reason)
		}
	}
	return nil
}

func (s *Server) integrationRelease(key, claim string, generation uint64, reason string) {
	if s.integrationConfig().Mode == config.IntegrationOff {
		return
	}
	store, err := s.integrationStore()
	if err == nil {
		err = store.Append(integration.Event{Version: integration.SchemaVersion, ID: fmt.Sprintf("release/%s/%d", key, time.Now().UnixNano()), At: time.Now(), Release: key, ReleaseClaim: claim, ReleaseGeneration: generation, Reason: reason})
	}
	if err != nil {
		s.webhookLogger().Error("integration reservation release failed", "work", key, "error", err)
	}
}

func (s *Server) registerIntegrationRoutes() {
	s.mux.HandleFunc("GET /api/integration-orchestration", s.handleIntegrationGet)
	s.mux.HandleFunc("PUT /api/config/integration-orchestration", s.handleIntegrationConfigPut)
	s.mux.HandleFunc("POST /api/integration-orchestration/intents", s.handleIntegrationIntent)
	s.mux.HandleFunc("POST /api/integration-orchestration/reconcile", s.handleIntegrationReconcile)
}

func (s *Server) handleIntegrationGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	cfg := s.integrationConfig()
	resp := map[string]any{"config": cfg, "schema_version": integration.SchemaVersion, "runtime_commit": versionHash,
		"source": "github-observations-v1", "enabled": cfg.Mode != config.IntegrationOff,
		"effective_writer":       "existing preparation owner or native GitHub queue; orchestration never writes merges",
		"native_group_evidence":  "unknown until complete authoritative group, tested tree and producer observations",
		"author_review_evidence": "separate; never substituted by integration evidence",
		"controls":               "Hive authoring admission only; no native queue jumping or GitHub build-window control"}
	store, err := s.integrationStore()
	if err != nil {
		resp["state"] = "unknown"
		resp["error"] = err.Error()
	} else {
		resp["ledger"] = store.Snapshot(time.Now())
	}
	jsonResponse(w, resp)
}

func (s *Server) handleIntegrationConfigPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var next config.IntegrationOrchestrationConfig
	if err := decodeBody(r, &next); err != nil {
		jsonError(w, "invalid configuration", http.StatusBadRequest)
		return
	}
	if err := next.Validate(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	s.integration.mu.Lock()
	defer s.integration.mu.Unlock()
	previous := s.deps.Config.IntegrationOrchestration
	// Reset only our reservations on every configuration transition, including
	// allowlist/bounds drift. Disabling must remain possible with a failed disk.
	store, ledgerErr := s.integrationStore()
	if ledgerErr == nil {
		ledgerErr = store.Append(integration.Event{Version: integration.SchemaVersion, ID: fmt.Sprintf("rollback/%d", time.Now().UnixNano()), At: time.Now(), Release: "*", Reason: "operator changed orchestration configuration"})
	}
	if ledgerErr != nil && next.Effective().Mode == config.IntegrationEnforce {
		jsonError(w, ledgerErr.Error(), http.StatusServiceUnavailable)
		return
	}
	s.deps.Config.IntegrationOrchestration = next
	if err := s.saveConfig(); err != nil {
		s.deps.Config.IntegrationOrchestration = previous
		jsonError(w, "integration configuration persistence failed", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_integration_orchestration", auditDetail("mode", next.Effective().Mode), "")
	if ledgerErr != nil {
		s.webhookLogger().Error("integration disabled; reservation cleanup unavailable, re-enablement fenced", "error", ledgerErr)
	}
	jsonResponse(w, next.Effective())
}

func (s *Server) handleIntegrationIntent(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var i integration.Intent
	if err := decodeBody(r, &i); err != nil {
		jsonError(w, "invalid intent", http.StatusBadRequest)
		return
	}
	if err := i.Validate(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.integrationConfig().Allows(i.Repo, i.Base) {
		jsonError(w, "intent outside enrolled scope", http.StatusBadRequest)
		return
	}
	if len(i.Prerequisites) > 0 {
		approved, deps := s.integrationPlan(i)
		if !approved || len(deps) != len(i.Prerequisites) {
			jsonError(w, "prerequisites must match bound nodes in the current approved plan DAG", http.StatusConflict)
			return
		}
	}
	store, err := s.integrationStore()
	if err == nil {
		err = store.Append(integration.Event{Version: integration.SchemaVersion, ID: fmt.Sprintf("intent/%s/%d", i.Key, i.Generation), At: i.ObservedAt, Intent: &i})
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	jsonResponse(w, i)
}
