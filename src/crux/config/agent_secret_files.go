package config

import (
	"path/filepath"
	"strings"
)

// ResolveAPIKey returns this gateway's key value, preferring the env var when
// set and falling back to the file. Returns "" when neither yields a value
// (a keyless endpoint). Mirrors LiteLLMConfig key resolution.
// secretFileRoots are the ONLY directories an api_key_file may live under.
//
//   - /secrets      — the read-only Kubernetes Secret projection
//   - /data/secrets — the PVC-backed dir the dashboard writes UI-entered keys to
//
// Anything else is refused. See SecretFilePathAllowed.
//
// A package var, not a const slice, so tests can point it at a t.TempDir()
// (production never reassigns it). Use SetSecretFileRootsForTest.
var secretFileRoots = []string{"/secrets", WritableSecretsDir}

// SecretFileRoots returns the directories hive reads secret files from, so a
// reader can redact their contents (the admin log read, #11942).
func SecretFileRoots() []string {
	return append([]string(nil), secretFileRoots...)
}

// SetSecretFileRootsForTest overrides the allowed secret-file roots and returns
// a function restoring the previous value. Intended for tests, which must write
// key files into a t.TempDir() rather than the real /secrets.
//
// This is not a security hole: it is compiled into the binary but never called
// outside tests, and anyone able to call it already has in-process code
// execution — at which point they can read the files directly.
func SetSecretFileRootsForTest(roots ...string) func() {
	prev := secretFileRoots
	secretFileRoots = roots
	return func() { secretFileRoots = prev }
}

// AllowSecretFileRoot registers an ADDITIONAL directory whose files may be read
// as a secret, returning a function that removes it again.
//
// This exists so a component that WRITES key files keeps its write location and
// this package's READ gate in lockstep. pkg/dashboard writes per-gateway keys to
// its own gatewaySecretsDir — a package var tests repoint at a temp dir — and
// without this the two seams disagree: the dashboard writes a key it can then
// never read back. Callers register the same directory they write to, so the
// gate stays a real confinement rather than something each test disables.
func AllowSecretFileRoot(dir string) func() {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return func() {}
	}
	prev := secretFileRoots
	secretFileRoots = append(append([]string(nil), secretFileRoots...), dir)
	return func() { secretFileRoots = prev }
}

// SecretFilePathAllowed reports whether p is a path hive may read a secret from.
//
// SECURITY (audit N8, CWE-200/918): api_key_file is attacker-controllable — the
// gateway upsert stores whatever absolute path it is given, and the save-time
// probe then reads that file and ships its contents to the gateway endpoint as
// an `Authorization: Bearer` header. Without confinement that is an arbitrary
// file read wired directly to an arbitrary outbound request: /data/secrets/*.pem
// (the GitHub App key), /proc/self/environ, token caches.
//
// The check is a prefix test against secretFileRoots, applied AFTER resolving
// symlinks, so a symlink inside an allowed directory cannot point out of it. A
// path that does not exist yet is still validated lexically — the dashboard
// legitimately writes a key file before anything reads it.
func SecretFilePathAllowed(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	// Resolve symlinks when the path (or its parent) exists, so a link planted
	// inside an allowed root cannot escape it. EvalSymlinks fails on a
	// not-yet-created file, in which case the lexical check below still applies.
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	} else if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(clean)); err == nil {
		clean = filepath.Join(resolvedDir, filepath.Base(clean))
	}
	for _, root := range secretFileRoots {
		rootClean := filepath.Clean(root)
		// Resolve the ROOT too. On macOS /var is a symlink to /private/var, so a
		// resolved path under a temp dir would never prefix-match an unresolved
		// root — the comparison has to be symlink-resolved on both sides or it
		// is inconsistent.
		if resolvedRoot, err := filepath.EvalSymlinks(rootClean); err == nil {
			rootClean = resolvedRoot
		}
		if clean == rootClean {
			continue // the directory itself is not a key file
		}
		// The separator suffix keeps "/data/secrets-evil" from matching the
		// "/data/secrets" root.
		if strings.HasPrefix(clean, rootClean+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
