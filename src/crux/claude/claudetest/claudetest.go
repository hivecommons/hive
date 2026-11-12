// Package claudetest holds test helpers for the Claude operator-token
// sources. It is test-support code only: nothing in a production package
// imports it.
package claudetest

import (
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/crux/claude"
)

// SetOperatorToken makes claude.OperatorToken hermetic for the test: every
// source the token can come from is pinned, so the result is exactly token
// (and the empty string leaves the long-lived token unset, which is the
// dashboard sign-in mode). The environment variable carries token, the
// _FILE variable is cleared and the default secret file is pointed at a path
// that does not exist, all restored when the test ends. It is the one place
// that knows the full list of sources, so tests do not drift from
// claude.OperatorToken as those change.
func SetOperatorToken(t testing.TB, token string) {
	t.Helper()
	t.Setenv(claude.OperatorTokenEnvVar, token)
	t.Setenv(claude.OperatorTokenFileEnvVar, "")
	orig := claude.OperatorTokenFile
	claude.OperatorTokenFile = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { claude.OperatorTokenFile = orig })
}
