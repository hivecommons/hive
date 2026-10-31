package dashboard

import (
	"context"
	"fmt"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"net/http"
	"reflect"
	"strings"
	"time"
)

func executionConfigEqual(a, b config.IntegrationExecutionConfig) bool {
	a, b = a.Effective(), b.Effective()
	if a.Mode == config.IntegrationOff && b.Mode == config.IntegrationOff && len(a.Scopes) == 0 && len(b.Scopes) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func executionParentChanged(a, b config.IntegrationOrchestrationConfig) bool {
	a.Execution, b.Execution = config.IntegrationExecutionConfig{}, config.IntegrationExecutionConfig{}
	return !reflect.DeepEqual(a, b)
}

func executionReleasedScopes(previous, next config.IntegrationExecutionConfig) []config.IntegrationExecutionScope {
	out := []config.IntegrationExecutionScope{}
	for _, old := range previous.Scopes {
		current, enrolled := executionEnrolledScope(next.Effective(), old.Repo, old.Base)
		if next.Effective().Mode != config.IntegrationEnforce || !enrolled || !reflect.DeepEqual(current, old) {
			out = append(out, old)
		}
	}
	return out
}

func (s *Server) executionHandoverGuard(ctx context.Context, previous, next config.IntegrationExecutionConfig) error {
	if previous.Effective().Mode != config.IntegrationEnforce {
		return nil
	}
	for _, old := range executionReleasedScopes(previous, next) {
		if old.Authority != "hive" {
			continue
		}
		current, enrolled := executionEnrolledScope(next, old.Repo, old.Base)
		if next.Effective().Mode == config.IntegrationEnforce && enrolled &&
			current.Authority == old.Authority && current.ExecutorID == old.ExecutorID &&
			current.AuthorityRef == old.AuthorityRef && current.AuthorityCommit == old.AuthorityCommit &&
			current.ProviderProfileDigest() == old.ProviderProfileDigest() {
			continue // Scheduling bounds alone do not hand promotion authority away.
		}
		if s.deps == nil || s.deps.GHClient == nil {
			return fmt.Errorf("revoke old forge authority epoch before releasing Hive promotion authority; live reader unavailable")
		}
		parts := strings.SplitN(old.Repo, "/", 2)
		ref, _, err := s.deps.GHClient.GoGitHub().Git.GetRef(ctx, parts[0], parts[1], strings.TrimPrefix(old.AuthorityRef, "refs/"))
		if err != nil {
			return fmt.Errorf("old authority revocation is unknown: %w", err)
		}
		if ref.GetObject().GetSHA() == old.AuthorityCommit || ref.GetObject().GetSHA() == "" {
			return fmt.Errorf("revoke exact old shared forge epoch before OFF/advisory/removal/authority handover; config alone cannot fence a paused provider sender")
		}
	}
	return nil
}

func (s *Server) handleIntegrationExecutionIdentity(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	_, err := s.integrationExecutionStore()
	if err != nil {
		jsonError(w, "execution identity unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, map[string]any{"executor_id": s.integration.executionIdentity.ID,
		"model": "designated-single-executor-no-failover", "enabled": false,
		"detail": "local create-only identity only; no registration, permissions, workflow dispatch or configuration enabled"})
}

func executionPublicProjection(store *integration.Store, cfg config.IntegrationExecutionConfig, now time.Time) map[string]any {
	return map[string]any{"schema_version": integration.ExecutionSchemaVersion, "ledger": store.ExecutionSnapshot(),
		"desired_mode": cfg.Effective().Mode, "runtime_mode": cfg.Effective().Mode,
		"runtime_version": versionHash, "source_version": "unknown", "published_image": "unknown",
		"authority_model":             "enrolled-participants designated-single-executor; no cloned identity, automatic failover or legacy Actions exactly-once",
		"unregistered_legacy_writers": "not fenced by a local configuration; stop/upgrade/enroll all participants before exclusive activation",
		"telemetry":                   "actual controlled producer/bootstrap plus author/native jobs, physical keys deduplicated; missing cohort is unknown",
		"observed_at":                 now}
}
