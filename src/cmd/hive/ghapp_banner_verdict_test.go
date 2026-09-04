package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// verdictTestKeyBits keeps key generation fast; these keys never leave the test.
const verdictTestKeyBits = 2048

// verdictTestAppID / verdictTestInstallationID are arbitrary non-placeholder
// identifiers — they only need to be non-zero for NewAppAuth to accept them.
const (
	verdictTestAppID          = 5686
	verdictTestInstallationID = 42980
)

func verdictTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// verdictTestAuth builds a real AppAuth backed by a freshly generated key on
// disk and pointed at a stub API, so classifyGitHubAppFailure exercises the
// same code path production does.
func verdictTestAuth(t *testing.T, apiURL string) *github.AppAuth {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, verdictTestKeyBits)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatalf("writing test key: %v", err)
	}
	// diagnoseGitHubApp pre-flights the well-known spoke key paths and reports
	// key-missing without any API call when none holds content. Point one of
	// them at the real key so these tests exercise the API classification the
	// verdict actually depends on. Restored via t.Cleanup.
	origProvisioned, origPVC := appKeys.ProvisionedKeyPath, appKeys.DataKeyPath
	appKeys.ProvisionedKeyPath, appKeys.DataKeyPath = keyPath, keyPath
	t.Cleanup(func() {
		appKeys.ProvisionedKeyPath, appKeys.DataKeyPath = origProvisioned, origPVC
	})
	auth, err := github.NewAppAuth(verdictTestAppID, verdictTestInstallationID, keyPath, verdictTestLogger(), apiURL)
	if err != nil {
		t.Fatalf("NewAppAuth: %v", err)
	}
	return auth
}

// TestClassifyGitHubAppFailure_UnknownDoesNotRaiseBanner is the regression test
// for the false banner: a hive whose first App probe fails for a reason we
// cannot classify (the API never answered) must NOT be told its App is broken.
// The boot path used to raise the banner unconditionally in this situation and
// then never lower it, while the Re-check button — running the identical probe
// — treated the same evidence as success and cleared it. That asymmetry is the
// entire "shows on load, disappears on Re-check" report.
func TestClassifyGitHubAppFailure_UnknownDoesNotRaiseBanner(t *testing.T) {
	// Port 1 is reserved and refuses connections, so no status is ever
	// returned and classification is genuinely inconclusive.
	auth := verdictTestAuth(t, "http://127.0.0.1:1")

	raise, msg, state := classifyGitHubAppFailure(context.Background(), auth, "katamari", verdictTestLogger())
	if raise {
		t.Error("an unreachable GitHub API must not raise the App banner")
	}
	if state != github.AppStateUnknown {
		t.Errorf("state = %s, want unknown when GitHub never answered", state)
	}
	if msg != "" {
		t.Errorf("msg = %q, want empty — we must not accuse anyone without a verdict", msg)
	}
	if state.OperatorActionable() || state.UserActionable() {
		t.Error("an unclassifiable failure must not be reported as anyone's fault")
	}
}

// TestClassifyGitHubAppFailure_RateLimitDoesNotRaiseBanner — GitHub answers a
// rate limit with 403, the same status a wrong installation returns. It is
// transient and must never latch a credential banner.
func TestClassifyGitHubAppFailure_RateLimitDoesNotRaiseBanner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "API rate limit exceeded"})
	}))
	defer srv.Close()

	raise, _, state := classifyGitHubAppFailure(context.Background(), verdictTestAuth(t, srv.URL), "katamari", verdictTestLogger())
	if raise {
		t.Error("a rate-limited probe must not raise the App banner")
	}
	if state != github.AppStateUnknown {
		t.Errorf("state = %s, want unknown for a rate-limited 403", state)
	}
}

// TestClassifyGitHubAppFailure_HealthyAppDoesNotRaiseBanner — the case the boot
// path got wrong. EnsureAdvisoryIssue can fail for repo-scoped reasons (the
// repo is archived, missing, or the issue write raced a permission refresh)
// while App auth itself is perfectly fine. Boot raised the banner on the error
// string alone; the verdict must come from the App probe.
func TestClassifyGitHubAppFailure_HealthyAppDoesNotRaiseBanner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          verdictTestInstallationID,
			"account":     map[string]any{"login": "katamari"},
			"permissions": map[string]any{"issues": "write"},
		})
	}))
	defer srv.Close()

	raise, msg, state := classifyGitHubAppFailure(context.Background(), verdictTestAuth(t, srv.URL), "katamari", verdictTestLogger())
	if raise {
		t.Errorf("a healthy App must not raise the banner (state=%s, msg=%q)", state, msg)
	}
	if state != github.AppStateOK {
		t.Errorf("state = %s, want ok", state)
	}
}

// TestClassifyGitHubAppFailure_GenuineFailureStillRaises guards against the fix
// silencing real problems: a 404 means the App truly is not installed, and that
// banner must still appear.
func TestClassifyGitHubAppFailure_GenuineFailureStillRaises(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
	}))
	defer srv.Close()

	raise, msg, state := classifyGitHubAppFailure(context.Background(), verdictTestAuth(t, srv.URL), "katamari", verdictTestLogger())
	if !raise {
		t.Error("a genuinely uninstalled App must still raise the banner")
	}
	if state != github.AppStateNotInstalled {
		t.Errorf("state = %s, want not-installed", state)
	}
	if msg == "" {
		t.Error("a raised banner must carry copy explaining what to fix")
	}
	if !state.UserActionable() {
		t.Error("not-installed is the user's to fix")
	}
}

// TestClassifyGitHubAppFailure_OperatorFaultIsNotBlamedOnUser — a 401 means the
// key does not belong to the App. Only the operator can fix that, and #2224
// exists so the user is never told to reinstall in this case.
func TestClassifyGitHubAppFailure_OperatorFaultIsNotBlamedOnUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "A JSON web token could not be decoded"})
	}))
	defer srv.Close()

	raise, _, state := classifyGitHubAppFailure(context.Background(), verdictTestAuth(t, srv.URL), "katamari", verdictTestLogger())
	if !raise {
		t.Error("an invalid key is a real fault and must raise the banner")
	}
	if state != github.AppStateKeyInvalid {
		t.Errorf("state = %s, want key-invalid", state)
	}
	if !state.OperatorActionable() || state.UserActionable() {
		t.Error("key-invalid is the operator's fault and must never be blamed on the user")
	}
}

// TestClassifyGitHubAppWriteForbidden_HealthyInstallSurfacesWriteFailure is the
// #2353 regression: the App authenticates, the installation resolves on the
// right account and grants issues:write (so diagnoseGitHubApp returns OK), yet a
// real write returned 403 "Resource not accessible by integration". Health must
// NOT stay silent (None) and must NOT be mislabeled "lacks Issues: Read &
// Write" — it must report the DISTINCT write-forbidden state with accurate copy
// naming the repo and the repo-scope cause.
func TestClassifyGitHubAppWriteForbidden_HealthyInstallSurfacesWriteFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GetInstallation reports a perfectly healthy installation: right owner,
		// issues:write granted. This is exactly the live case from #2353.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          verdictTestInstallationID,
			"account":     map[string]any{"login": "open-horizon-services"},
			"permissions": map[string]any{"issues": "write"},
		})
	}))
	defer srv.Close()

	msg, state := classifyGitHubAppWriteForbidden(
		context.Background(),
		verdictTestAuth(t, srv.URL),
		"open-horizon-services",
		"Getting-Started",
	)
	if state == github.AppStateOK || state == github.AppStateUnknown {
		t.Fatalf("a write-403 on a healthy install must not stay silent; state=%s", state)
	}
	if state == github.AppStateInsufficientPerms {
		t.Error("must NOT be mislabeled as an Issues permission gap — the permission is granted")
	}
	if state != github.AppStateWriteForbidden {
		t.Errorf("state = %s, want write-forbidden", state)
	}
	if msg == "" {
		t.Error("write-forbidden must carry an accurate, non-empty message")
	}
	if !strings.Contains(msg, "Getting-Started") {
		t.Errorf("message must name the repo; got %q", msg)
	}
	if strings.Contains(msg, "lacks Issues") {
		t.Errorf("message must not fabricate a permission gap; got %q", msg)
	}
}

// TestClassifyGitHubAppWriteForbidden_RealAuthProblemWins — when the write 403
// coincides with a GENUINE App-auth fault (here: a 401 key-invalid), report that
// real cause rather than masking it behind the repo-scope story.
func TestClassifyGitHubAppWriteForbidden_RealAuthProblemWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "A JSON web token could not be decoded"})
	}))
	defer srv.Close()

	_, state := classifyGitHubAppWriteForbidden(
		context.Background(),
		verdictTestAuth(t, srv.URL),
		"open-horizon-services",
		"Getting-Started",
	)
	if state != github.AppStateKeyInvalid {
		t.Errorf("state = %s, want key-invalid — the real fault must win", state)
	}
}

// TestIsGitHubRateLimitText covers the one place error-string matching survives
// — where it can only ever cause an extra correct classification, never an
// accusation.
func TestIsGitHubRateLimitText(t *testing.T) {
	if isGitHubRateLimitText(nil) {
		t.Error("nil is not a rate limit")
	}
	for _, s := range []string{"API rate limit exceeded", "secondary RATE LIMIT hit"} {
		if !isGitHubRateLimitText(errString(s)) {
			t.Errorf("%q should be detected as a rate limit", s)
		}
	}
	if isGitHubRateLimitText(errString("403 Resource not accessible by integration")) {
		t.Error("a plain 403 is not a rate limit")
	}
}

// errString is a minimal error carrying exactly the message given.
type errString string

func (e errString) Error() string { return string(e) }
