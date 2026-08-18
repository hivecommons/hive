package repair

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/scheduler"
)

func TestGovernedSourceComposerBindsSchedulerToWorkerSealedTree(t *testing.T) {
	repository, _ := seedGitRepository(t)
	baseSHA := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	baseTreeSHA := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
	evidenceRoot := filepath.Join(t.TempDir(), ".visual-hive")
	if err := os.MkdirAll(evidenceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestSHA := strings.Repeat("1", 64)
	receiptSHA := strings.Repeat("2", 64)
	allowed := []string{"src/**"}
	contracts := []string{"contract/value"}
	composer, err := NewGovernedSourceContextComposer(GovernedSourceContextOptions{
		Worktree: repository, Repository: "owner/repo", BaseSHA: baseSHA, BaseTreeSHA: baseTreeSHA,
		EvidenceRoot: evidenceRoot, AllowedPaths: allowed, AffectedContracts: contracts,
		ManifestSHA256: manifestSHA, VerificationReceiptSHA256: receiptSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = composer.Close() })

	// The proposal context must still come from the sealed Git tree when the
	// checkout changes after Worker has bound the exact base.
	if err := os.WriteFile(filepath.Join(repository, "src", "value.txt"), []byte("unsealed checkout bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := scheduler.WorkerSourceContextRequest{
		Repository: "owner/repo", BaseSHA: baseSHA, BaseTreeSHA: baseTreeSHA,
		AllowedPaths: allowed, AffectedContracts: contracts,
		ManifestSHA256: manifestSHA, VerificationReceiptSHA256: receiptSHA,
	}
	sealed, err := composer.ComposeGovernedSourceContext(request)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.BaseSHA != baseSHA || sealed.BaseTreeSHA != baseTreeSHA || sealed.ManifestSHA256 != manifestSHA || sealed.VerificationReceiptSHA256 != receiptSHA ||
		len(sealed.Files) != 1 || sealed.Files[0].Path != "src/value.txt" || !strings.Contains(sealed.Content, "broken\\n") || strings.Contains(sealed.Content, "unsealed checkout bytes") {
		t.Fatalf("sealed source context lost its exact bindings: %+v\n%s", sealed, sealed.Content)
	}

	changed := request
	changed.AllowedPaths = []string{"**/*"}
	if _, err := composer.ComposeGovernedSourceContext(changed); err == nil {
		t.Fatal("Scheduler expanded Worker-owned source authorization")
	}
	changed = request
	changed.VerificationReceiptSHA256 = strings.Repeat("3", 64)
	if _, err := composer.ComposeGovernedSourceContext(changed); err == nil {
		t.Fatal("Scheduler changed the admitted verification receipt binding")
	}
}
