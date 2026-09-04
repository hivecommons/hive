package appkey

import (
	"path/filepath"
	"testing"
)

// testKeys mirrors the package-level path vars these tests mutated when this
// code lived in package main: the tests were moved here unchanged apart from a
// mechanical rename onto this Resolver, so their save/restore patterns and
// assertions are byte-for-byte the same logic they were before the move
// (hivecommons/hive#5898 phase 1).
var testKeys = Default()

// isolateAppKeyLookup points the shared test Resolver at an empty temp dir so a
// test starts from "no key anywhere". Duplicated from cmd/hive's copy when this
// logic moved here (hivecommons/hive#5898 phase 1): both packages now have
// tests that need the isolation, and a ten-line test helper is not worth an
// exported testing surface.
func isolateAppKeyLookup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origData, origSecrets := testKeys.DataKeyPath, testKeys.ProvisionedKeyPath
	testKeys.DataKeyPath = filepath.Join(dir, "absent-data-key.pem")
	testKeys.ProvisionedKeyPath = filepath.Join(dir, "absent-secrets-key.pem")
	t.Setenv("GH_APP_KEY_FILE", "")
	t.Cleanup(func() {
		testKeys.DataKeyPath, testKeys.ProvisionedKeyPath = origData, origSecrets
	})
	return dir
}
