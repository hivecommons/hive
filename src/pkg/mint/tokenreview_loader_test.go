package mint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the projected-token loader and the in-cluster constructor's CA
// branch — the pieces TestNewInClusterTokenReviewAuthenticatorRefusesOutsideACluster
// does not reach.
//
// readFileTrimmed is what lets the mint keep authenticating ITSELF after the
// kubelet rotates the projected ServiceAccount token on disk: it must re-read
// on every call and strip the trailing newline the projection writes. A loader
// that cached, or that passed the newline through, would start failing reviews
// an hour after boot while looking healthy.

func TestReadFileTrimmedRereadsRotatedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  first-token\n"), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}

	load := readFileTrimmed(path)

	got, err := load()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if got != "first-token" {
		t.Errorf("first load = %q, want %q (whitespace must be trimmed)", got, "first-token")
	}

	// Rotate the token on disk. The loader must observe the new value: a
	// cached read here is the bug the doc comment on readFileTrimmed warns
	// about — the process stays healthy while every review starts failing.
	if err := os.WriteFile(path, []byte("second-token\n"), 0o600); err != nil {
		t.Fatalf("rotating token: %v", err)
	}
	got, err = load()
	if err != nil {
		t.Fatalf("load after rotation: %v", err)
	}
	if got != "second-token" {
		t.Errorf("load after rotation = %q, want %q (loader must re-read, not cache)", got, "second-token")
	}
}

func TestReadFileTrimmedPropagatesMissingFile(t *testing.T) {
	load := readFileTrimmed(filepath.Join(t.TempDir(), "does-not-exist"))
	if _, err := load(); err == nil {
		t.Error("loading a missing token file succeeded — the error must propagate so review failures are attributable")
	}
}

func TestReadFileTrimmedEmptyFileYieldsEmptyToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("\n\t \n"), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}
	got, err := readFileTrimmed(path)()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "" {
		t.Errorf("whitespace-only file loaded as %q, want empty string", got)
	}
}

// The in-cluster constructor must fail rather than degrade when the
// KUBERNETES_SERVICE_* environment is present but the mounted CA bundle is
// not readable — a half-present cluster environment is a misconfiguration,
// not a cue to fall back to weaker trust.
//
// The assertion is conditional on whether the projected CA path exists so the
// test stays hermetic both on plain hosts (the common case: the path is
// absent, construction must fail naming the CA) and inside a real pod (the
// path exists, construction must succeed against the fake host/port because
// no network I/O happens at construction time).
func TestNewInClusterTokenReviewAuthenticatorCABranch(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "kubernetes.default.svc.hive.invalid")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	a, err := NewInClusterTokenReviewAuthenticator(testAudience)

	if _, statErr := os.Stat(inClusterCAPath); statErr != nil {
		// Plain host: the CA bundle is absent, so construction must refuse.
		if err == nil {
			t.Fatal("built an in-cluster authenticator with no readable CA bundle — it must fail rather than degrade to host trust")
		}
		if !strings.Contains(err.Error(), "CA bundle") {
			t.Errorf("error = %q, want it to name the CA bundle so the operator knows what is missing", err)
		}
		return
	}

	// Real pod: the projected CA exists, so construction succeeds without
	// touching the network.
	if err != nil {
		t.Fatalf("NewInClusterTokenReviewAuthenticator with a present CA bundle: %v", err)
	}
	if a == nil {
		t.Fatal("nil authenticator with nil error")
	}
}
