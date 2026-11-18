package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/reviewrules"
)

func reviewRuleVaultFixture(t *testing.T, vault string) (*boot, *knowledge.KnowledgeAPI, *knowledge.Primer) {
	t.Helper()
	t.Setenv(reviewrules.VaultEnvVar, vault)
	t.Setenv(reviewrules.DisableEnvVar, "")
	logger := discardLogger()
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)
	p := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25}, logger)
	b := &boot{logger: logger}
	b.registerReviewRuleVault(api, p)
	return b, api, p
}

func captureFixtureRule(t *testing.T, vault string) {
	t.Helper()
	rs, err := reviewrules.Open(t.TempDir(), vault, reviewrules.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if res := rs.CaptureComment(reviewrules.Comment{Repo: "cncf/techdocs", PR: 424, CommentID: "inline:1001", URL: fixtureCommentURL,
		Reviewer: "nate-double-u", Association: "OWNER", Lane: "quality", Body: maintainersFixture}, time.Now()); res.Outcome != reviewrules.OutcomeCaptured {
		t.Fatalf("capture = %+v", res)
	}
}

func primedFor(p *knowledge.Primer, lane string) string {
	out, _ := p.PrimeForKick(context.Background(), nil, knowledge.KickContext{Lane: lane}, nil).Render()
	return out
}

// A host with no vaults directory: the channel does not exist at boot, the
// sweep creates it on the first capture, and its rule is primed and the
// channel connected on the next kick, with no restart (#19094).
func TestRegisterReviewRuleVault_CreatedOnFirstCaptureIsPrimedWithoutRestart(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "no-vaults-dir", "review-rules")
	_, api, p := reviewRuleVaultFixture(t, vault)
	if !p.HasPinned() {
		t.Fatal("the channel must be registered by path even before it exists")
	}
	if _, err := os.Stat(vault); err == nil {
		t.Fatal("boot must not create a vaults directory the host does not have")
	}
	if api.GetVaultStore(vault) != nil {
		t.Fatal("nothing to connect before the first capture")
	}
	if got := primedFor(p, "quality"); got != "" {
		t.Fatalf("no rules yet, got:\n%s", got)
	}

	captureFixtureRule(t, vault)

	if got := primedFor(p, "quality"); !strings.Contains(got, maintainersRuleText) {
		t.Fatalf("a rule captured after boot must reach the next quality kick:\n%s", got)
	}
	if api.GetVaultStore(vault) == nil {
		t.Fatal("the channel must be connected for the dashboard once it exists")
	}
	if got := primedFor(p, "quality"); strings.Count(got, maintainersRuleText) != 1 {
		t.Fatalf("the rule is primed once per kick:\n%s", got)
	}
}

// Beside the hive's other vaults the channel is created and connected at boot.
func TestRegisterReviewRuleVault_CreatedBesideOtherVaultsAtBoot(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "review-rules")
	_, api, p := reviewRuleVaultFixture(t, vault)
	if api.GetVaultStore(vault) == nil || !p.HasPinned() {
		t.Fatal("an existing vaults directory gets the channel connected and pinned at boot")
	}
	captureFixtureRule(t, vault)
	api.GetVaultStore(vault).Reindex()
	if got := primedFor(p, "quality"); !strings.Contains(got, maintainersRuleText) {
		t.Fatalf("quality kick:\n%s", got)
	}
}

func TestRegisterReviewRuleVault_DisabledRegistersNothing(t *testing.T) {
	t.Setenv(reviewrules.VaultEnvVar, filepath.Join(t.TempDir(), "review-rules"))
	t.Setenv(reviewrules.DisableEnvVar, "off")
	logger := discardLogger()
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)
	p := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25}, logger)
	(&boot{logger: logger}).registerReviewRuleVault(api, p)
	if p.HasPinned() {
		t.Fatal("capture off: nothing pinned")
	}
}

func TestParentExists(t *testing.T) {
	dir := t.TempDir()
	if !parentExists(filepath.Join(dir, "x")) || parentExists(filepath.Join(dir, "missing", "x")) {
		t.Fatal("parentExists")
	}
}
