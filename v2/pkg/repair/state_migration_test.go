package repair

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyPROpenMigrationRecoversOnlyCompleteLifecycleBinding(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"schema_version":"hive.repair-worker-state.v3","attempts":{"complete":{"repository":"owner/repo","repository_fingerprint":"complete","attempt":2,"branch":"hive/repair-complete-a2","worktree":"worktree","stage":"pr_open","provider":"codex","commit_sha":"0123456789012345678901234567890123456789","pr_number":19,"pr_url":"https://github.com/owner/repo/pull/19","started_at":"2026-07-10T00:00:00Z","updated_at":"2026-07-10T00:00:00Z"},"incomplete":{"repository":"owner/repo","repository_fingerprint":"incomplete","attempt":2,"branch":"hive/repair-incomplete-a2","worktree":"worktree","stage":"pr_open","provider":"codex","commit_sha":"1123456789012345678901234567890123456789","pr_number":20,"started_at":"2026-07-10T00:00:00Z","updated_at":"2026-07-10T00:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(dir, "repair-worker-state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	complete, _ := store.Get("complete")
	incomplete, _ := store.Get("incomplete")
	if !complete.LifecyclePROpen || !complete.AttemptCounted {
		t.Fatalf("complete legacy PR binding was not recovered: %+v", complete)
	}
	if incomplete.LifecyclePROpen {
		t.Fatalf("incomplete legacy PR binding was incorrectly recovered: %+v", incomplete)
	}
}
