package claudetest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/claude/claudetest"
)

// SetOperatorToken pins every source claude.OperatorToken reads: with a
// token staged in the env, the _FILE variable and the default secret file,
// the helper makes the result exactly the token it was given, and "" reads
// as unset even though both files would otherwise supply one.
func TestSetOperatorTokenPinsEverySource(t *testing.T) {
	dir := t.TempDir()
	named := filepath.Join(dir, "named")
	if err := os.WriteFile(named, []byte("sk-ant-oat01-from-named-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(dir, "default")
	if err := os.WriteFile(def, []byte("sk-ant-oat01-from-default-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(claude.OperatorTokenEnvVar, "sk-ant-oat01-from-env")
	t.Setenv(claude.OperatorTokenFileEnvVar, named)
	orig := claude.OperatorTokenFile
	claude.OperatorTokenFile = def
	t.Cleanup(func() { claude.OperatorTokenFile = orig })

	t.Run("unset", func(t *testing.T) {
		claudetest.SetOperatorToken(t, "")
		if got := claude.OperatorToken(); got != "" {
			t.Fatalf("OperatorToken() = %q, want unset", got)
		}
		if claude.AuthMode() != claude.AuthModeDashboardSignIn {
			t.Fatalf("AuthMode() = %q", claude.AuthMode())
		}
	})
	t.Run("set", func(t *testing.T) {
		claudetest.SetOperatorToken(t, "sk-ant-oat01-staged")
		if got := claude.OperatorToken(); got != "sk-ant-oat01-staged" {
			t.Fatalf("OperatorToken() = %q", got)
		}
	})

	// Cleanup ran for both subtests: the outer staging is back.
	if claude.OperatorTokenFile != def {
		t.Fatalf("default secret file not restored: %q", claude.OperatorTokenFile)
	}
	if got := claude.OperatorToken(); got != "sk-ant-oat01-from-env" {
		t.Fatalf("env staging not restored: OperatorToken() = %q", got)
	}
}
