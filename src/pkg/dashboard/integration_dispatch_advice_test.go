package dashboard

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

func TestIntegrationRealAdvisoryDispatchExposesLiveSemanticAdvice(t *testing.T) {
	_, server, ledger := claimsHub(t)
	server.integration.path = filepath.Join(t.TempDir(), "integration.jsonl")
	server.deps.Config.IntegrationOrchestration = config.IntegrationOrchestrationConfig{Mode: config.IntegrationAdvisory, Scopes: []config.IntegrationScope{{Repo: "myorg/repo1", Base: "v5"}}}
	store, err := server.integrationStore()
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []int{1, 2} {
		intent := integration.Intent{Version: 1, Key: fmt.Sprintf("myorg/repo1#%d", number), Repo: "myorg/repo1", Base: "v5", Generation: 1, Plan: "unresolved", PlanNode: "owner-enrolled", ScopeKnown: true, Provenance: "owner", ObservedAt: time.Now(), Contracts: []string{"wire/v1"}, Paths: []string{fmt.Sprintf("%d.go", number)}}
		if err := store.EnsureIntent(intent); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := ledger.Claim(claims.Request{Repo: "myorg/repo1", Issue: 2, Holder: "agent", Kind: claims.KindAgent}); err != nil || result.Outcome != claims.OutcomeClaimed {
		t.Fatalf("shadow blocked authoritative ownership: %+v %v", result, err)
	}
	raw := &ghpkg.ActionableResult{Issues: ghpkg.IssueResultFromItems([]ghpkg.Issue{{Repo: "myorg/repo1", Number: 1}})}
	if server.IntegrationKickAdmission(raw) != raw {
		t.Fatal("advisory changed dispatch")
	}
	found := false
	for _, observation := range server.integrationDispatchAdvice() {
		if observation.Key == "myorg/repo1#1" {
			found = true
			if len(observation.Decision.Hints) != 1 || !strings.Contains(observation.Decision.Hints[0], "semantic interaction") || !observation.Decision.Allowed {
				t.Fatalf("actual dispatch advice lost: %+v", observation)
			}
		}
	}
	if !found || len(store.Snapshot(time.Now()).Reservations) != 0 {
		t.Fatal("shadow advice absent or reserved work")
	}
}

func TestIntegrationDispatchAdviceIsBoundedAndCopiesHints(t *testing.T) {
	server := orchestrationServer(t, config.IntegrationAdvisory)
	ref := worksource.Ref{Repo: "o/r", Number: 1}
	hints := []string{"semantic"}
	server.rememberIntegrationDispatch(ref, "v5", config.IntegrationAdvisory, integration.Decision{Allowed: true, Hints: hints})
	hints[0] = "mutated"
	snapshot := server.integrationDispatchAdvice()
	if len(snapshot) != 1 || snapshot[0].Decision.Hints[0] != "semantic" {
		t.Fatal("input advice aliases cache")
	}
	snapshot[0].Decision.Hints[0] = "mutated"
	if server.integrationDispatchAdvice()[0].Decision.Hints[0] != "semantic" {
		t.Fatal("output advice aliases cache")
	}
	for n := 2; n <= integrationDispatchObservationLimit+2; n++ {
		server.rememberIntegrationDispatch(worksource.Ref{Repo: "o/r", Number: n}, "v5", config.IntegrationAdvisory, integration.Decision{Allowed: true})
	}
	if len(server.integrationDispatchAdvice()) != integrationDispatchObservationLimit {
		t.Fatal("unbounded dispatch diagnostics")
	}
}
