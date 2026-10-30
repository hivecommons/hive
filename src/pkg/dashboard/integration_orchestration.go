package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
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
	mu                   sync.Mutex
	once                 sync.Once
	store                *integration.Store
	err                  error
	path                 string
	lastReconcile        time.Time
	reconcileScopeOffset int
	advice               map[string]integrationDispatchObservation
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
	key := ""
	for _, intent := range store.Intents() {
		if strings.EqualFold(intent.Repo, repo) && intent.Base == pr.GetBase().GetRef() && intent.Head == pr.GetHead().GetSHA() {
			if key != "" && key != intent.Key {
				if cfg.Mode == config.IntegrationEnforce {
					return "native candidate has ambiguous canonical work intent", nil
				}
				return "", nil
			}
			key = intent.Key
		}
	}
	in := s.integrationInput(key, repo, pr.GetBase().GetRef(), time.Now())
	in.Head = pr.GetHead().GetSHA()
	if cfg.Mode == config.IntegrationEnforce && key != "" {
		in.Reserve = true
		in.Claim, in.ClaimGeneration, in.ClaimExpiresAt = "native-candidate/"+pr.GetNodeID()+"/"+in.Head, 1, in.Now.Add(cfg.LeaseTTL)
		if ref, valid := worksource.ParseKey(key); valid {
			if ledger := s.claimsLedger(); ledger != nil {
				if claim, live := ledger.Lookup(ref.Repo, ref.Number); live {
					in.Claim, in.ClaimGeneration, in.ClaimExpiresAt = claim.Holder, uint64(claim.ClaimedAt.UnixNano()), claim.ExpiresAt
				}
			}
			if reservation, exists := store.Reservation(key, in.Now); exists {
				in.Claim, in.ClaimGeneration, in.ClaimExpiresAt = reservation.Claim, reservation.ClaimGeneration, reservation.ExpiresAt
			}
		}
	}
	d, err := store.Decide(cfg, in)
	if err != nil && cfg.Mode == config.IntegrationEnforce {
		return "admission evaluation unavailable", err
	}
	if !d.Allowed {
		return d.Reason, nil
	}
	return "", nil
}

func (s *Server) IntegrationObserveFiles(repo, head string, files []string) {
	cfg := s.integrationConfig()
	if cfg.Mode == config.IntegrationOff {
		return
	}
	enrolled := false
	for _, scope := range cfg.Scopes {
		if strings.EqualFold(scope.Repo, repo) {
			enrolled = true
			break
		}
	}
	if !enrolled {
		return
	}
	store, err := s.integrationStore()
	if err == nil {
		err = store.ObserveFiles(repo, head, files, time.Now())
	}
	if err != nil {
		s.webhookLogger().Warn("integration scope drift unavailable; overlap planner unaffected", "repo", repo, "error", err)
	}
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
		if s.integration.err == nil && s.integrationConfig().Mode != config.IntegrationOff {
			s.integration.err = s.integration.store.SetPolicies(s.integrationConfig().Policies, time.Now())
		}
	})
	return s.integration.store, s.integration.err
}

func integrationBeadWorkKey(b *beads.Bead) string {
	if b == nil {
		return ""
	}
	key := b.Meta(planning.MetaIssueRepo) + "#" + b.Meta(planning.MetaIssueNumber)
	if _, valid := worksource.ParseKey(key); valid {
		return key
	}
	key = strings.TrimPrefix(b.ExternalRef, "gh-")
	if _, valid := worksource.ParseKey(key); valid {
		return key
	}
	return ""
}

func (s *Server) integrationDispatchIntent(ref worksource.Ref, base string, now time.Time) integration.Intent {
	intent := integration.Intent{Version: integration.SchemaVersion, Key: ref.Key(), Repo: ref.Repo, Base: base, Generation: 1,
		Plan: "unresolved", PlanNode: ref.Key(), ScopeKnown: false, Confidence: "unknown", Provenance: "dispatch-enumerator", ObservedAt: now}
	if s.deps == nil {
		return intent
	}
	for _, store := range s.deps.BeadStores {
		if store == nil {
			continue
		}
		bead := store.FindByExternalRef("gh-" + ref.Key())
		if bead == nil {
			bead = store.FindByExternalRef(ref.Key())
		}
		if bead == nil || integrationBeadWorkKey(bead) != ref.Key() {
			continue
		}
		parent := bead.Meta(planning.MetaParentEpic)
		tree, found := planning.FindPlanTree(s.deps.BeadStores, parent)
		if !found || !tree.Approved {
			continue
		}
		if intent.Plan != "unresolved" && (intent.Plan != tree.EpicID || intent.PlanNode != bead.ID) {
			intent.Plan, intent.PlanNode, intent.Confidence = "unresolved", ref.Key(), "ambiguous-approved-plan"
			intent.Prerequisites = nil
			return intent
		}
		intent.Plan, intent.PlanNode, intent.PlanRevision = tree.EpicID, bead.ID, tree.DesignRevision
		intent.Confidence = "approved-dag-linked-scope-unknown"
		for _, node := range tree.Children {
			if node.ID != bead.ID {
				continue
			}
			for _, dependencyID := range node.DependsOn {
				dependency, err := store.Get(dependencyID)
				if err != nil {
					continue
				}
				if key := integrationBeadWorkKey(dependency); key != "" {
					intent.Prerequisites = append(intent.Prerequisites, integration.Prerequisite{Key: key, PlanNode: dependencyID, Condition: "integrated"})
				}
			}
		}
	}
	return intent
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
		if err == nil && integrationBeadWorkKey(b) == i.Key {
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
	if len(edges) != len(i.Prerequisites) {
		return false, deps
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
			if err == nil && integrationBeadWorkKey(b) == p.Key {
				keyBound = true
				break
			}
		}
		if keyBound {
			dependency := integration.Dependency{}
			switch p.Condition {
			case "developed":
				dependency = integration.Dependency{Known: true, Satisfied: child.Status == beads.StatusDone || child.Status == beads.StatusClosed}
			case "", "integrated":
				link, err := url.Parse(child.PRURL)
				if err == nil && link.Host == "github.com" {
					parts := strings.Split(strings.Trim(link.Path, "/"), "/")
					if len(parts) == 4 && parts[2] == "pull" {
						number, parseErr := strconv.Atoi(parts[3])
						ledger, ledgerErr := s.integrationStore()
						if parseErr == nil && ledgerErr == nil {
							_, merged := ledger.Integrated(parts[0]+"/"+parts[1], number)
							dependency = integration.Dependency{Known: merged, Satisfied: merged}
						}
					}
				}
			case "deployed": // No deployment producer is configured: closure is not deployment.
			}
			deps[p.Key] = dependency
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
	if i, exists := store.Intent(key); exists {
		in.IntentGeneration = i.Generation
		in.PlanApproved, in.Dependencies = s.integrationPlan(i)
		in.PlanRevision = i.PlanRevision
		if ref, valid := worksource.ParseKey(key); valid {
			actual := s.integrationDispatchIntent(ref, base, now)
			in.PlanRequired = actual.Plan != "unresolved"
			if in.PlanRequired && (actual.Plan != i.Plan || actual.PlanNode != i.PlanNode || actual.PlanRevision != i.PlanRevision) {
				in.PlanApproved = false
			}
		}
	}
	return in
}

// integrationDispatchDecision is shared by the internal and contributor lanes.
// The scope's base is explicit because issue enumerations do not carry a base.
// An ambiguous multi-base issue is not silently assigned to either cohort.
func (s *Server) integrationDispatchDecision(ref worksource.Ref) (out integration.Decision) {
	cfg := s.integrationConfig()
	out = integration.Decision{Allowed: true}
	if cfg.Mode == config.IntegrationOff {
		return out
	}
	bases := []string{}
	for _, scope := range cfg.Scopes {
		if !strings.EqualFold(scope.Repo, ref.Repo) {
			continue
		}
		bases = append(bases, scope.Base)
	}
	if len(bases) == 0 {
		return out
	}
	base := ""
	if len(bases) == 1 {
		base = bases[0]
	}
	defer func() { s.rememberIntegrationDispatch(ref, base, cfg.Mode, out) }()
	store, err := s.integrationStore()
	if err != nil {
		out.Reason = "integration ledger unavailable: " + err.Error()
		out.WouldBlock, out.Allowed = true, cfg.Mode != config.IntegrationEnforce
		return out
	}
	if len(bases) > 1 {
		intent, known := store.Intent(ref.Key())
		if !known || !cfg.Allows(ref.Repo, intent.Base) {
			out.Reason = "issue base unknown across multiple enrolled bases"
			out.WouldBlock, out.Allowed = true, cfg.Mode != config.IntegrationEnforce
			return out
		}
		base = intent.Base
	}
	if ref.Key() != "" {
		now := time.Now()
		intent := s.integrationDispatchIntent(ref, base, now)
		if err := store.EnsureIntent(intent); err != nil {
			return integration.Decision{Allowed: cfg.Mode != config.IntegrationEnforce, WouldBlock: true, Reason: "intent persistence unavailable: " + err.Error()}
		}
	}
	in := s.integrationInput(ref.Key(), ref.Repo, base, time.Now())
	if ledger := s.claimsLedger(); ledger != nil {
		for _, claim := range ledger.List() {
			if strings.EqualFold(claim.Repo, ref.Repo) {
				in.LiveKeys = append(in.LiveKeys, claim.Key())
			}
		}
	}
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
		if intent, known := store.Intent(c.Key()); known && intent.Base != scope.Base {
			continue
		}
		in := s.integrationInput(c.Key(), c.Repo, scope.Base, time.Now())
		in.Claim, in.ClaimGeneration, in.ClaimExpiresAt, in.Reserve = c.Holder, uint64(c.ClaimedAt.UnixNano()), c.ExpiresAt, true
		in.RenewOnly = true
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
		if intent, known := store.Intent(c.Key()); known && intent.Base != scope.Base {
			continue
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

func (s *Server) integrationLease(key, repo, identity string, gen uint64, expires, now time.Time, renewal bool) error {
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
		if intent, known := store.Intent(key); known && intent.Base != scope.Base {
			continue
		}
		in := s.integrationInput(key, repo, scope.Base, now)
		in.Reserve, in.Claim, in.ClaimGeneration, in.ClaimExpiresAt = true, "contributor/"+identity, gen, expires
		in.RenewOnly = renewal
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
		"source": "github-observations-v1", "source_version": 1, "enabled": cfg.Mode != config.IntegrationOff,
		"effective_writer":         "unknown in this observer; the existing live preparation/native authority guard remains authoritative",
		"native_group_evidence":    "unknown until complete authoritative group, tested tree and producer observations",
		"author_review_evidence":   "separate; never substituted by integration evidence",
		"direct_producer_contract": "hive-integration-subject-v1-<attempt>/subject.json from the pinned workflow; no producer is installed or activated by this feature",
		"proof_semantics":          "historical one-workflow receipts only; never aggregate merge eligibility",
		"preparation_source":       "/api/stage-tracker.base_preparation; ownership/backlog, not ready train or native queue seats",
		"reconciliation":           map[string]any{"interval_seconds": integrationReconcileInterval.Seconds(), "max_runs_per_scope": integrationReconcileMaxRuns, "max_job_reads": integrationReconcileMaxJobReads, "max_policy_reads": integrationReconcileMaxPolicyReads, "cohort": "bounded repository listing; overflow is unknown, not zero"},
		"controls":                 "Hive authoring and new Hive-owned enqueue admission only; no native queue jumping or GitHub build-window control"}
	scopes := make([]map[string]any, 0, len(cfg.Scopes))
	for _, scope := range cfg.Scopes {
		status := map[string]any{"repo": scope.Repo, "base": scope.Base, "live_authority_state": "unknown; configured is not effective"}
		if s.deps != nil && s.deps.Config != nil {
			status["preparation_configured"] = s.deps.Config.AutoMerge.RequireCurrentBaseFor(scope.Repo)
			status["native_configured"] = s.deps.Config.AutoMerge.NativeMergeQueue.EnabledFor(scope.Repo)
		}
		scopes = append(scopes, status)
	}
	resp["configured_authorities"] = scopes
	bindings := make([]map[string]any, 0, len(cfg.Policies))
	for _, policy := range cfg.Policies {
		bindings = append(bindings, map[string]any{"policy": policy, "fingerprint": policy.Fingerprint()})
	}
	resp["policy_bindings"] = bindings
	resp["dispatch_advice"] = s.integrationDispatchAdvice()
	resp["dispatch_advice_contract"] = "last actual internal/contributor evaluations; bounded boot-local diagnostics, not durable merge proof"
	resp["aging_policy"] = "oldest intent within existing priority/own-work bands; per-repository/base active and budget bounds; no native reordering"
	store, err := s.integrationStore()
	if err != nil {
		resp["state"] = "unknown"
		resp["error"] = err.Error()
	} else {
		now := time.Now()
		snapshot := store.Snapshot(now)
		resp["ledger"] = snapshot
		reports := make([]map[string]any, 0, len(cfg.Scopes))
		for _, scope := range cfg.Scopes {
			through := time.Time{}
			for _, coverage := range snapshot.Coverage {
				if strings.EqualFold(coverage.Repo, scope.Repo) && coverage.Base == scope.Base {
					through = coverage.Through
				}
			}
			stale := through.IsZero() || through.After(now) || now.Sub(through) > cfg.MaxObservationAge
			if through.IsZero() {
				through = now
			}
			reports = append(reports, map[string]any{"stale": stale, "report": store.CostReport(scope.Repo, scope.Base, through.Add(-cfg.Window), through)})
		}
		resp["ci_accounting"] = reports
		if source := s.hotspotSourceRef(); source != nil {
			resp["existing_hotspots"] = source.HotspotsJSON("")
		}
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
	if ledgerErr == nil {
		ledgerErr = store.SetPolicies(next.Policies, time.Now())
	}
	if ledgerErr != nil && next.Effective().Mode == config.IntegrationEnforce {
		jsonError(w, ledgerErr.Error(), http.StatusServiceUnavailable)
		return
	}
	s.deps.Config.IntegrationOrchestration = next
	if err := s.saveConfig(); err != nil {
		s.deps.Config.IntegrationOrchestration = previous
		if ledgerErr == nil {
			if restoreErr := store.SetPolicies(previous.Policies, time.Now()); restoreErr != nil {
				s.webhookLogger().Error("integration producer policy rollback unavailable", "error", restoreErr)
			}
		}
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
