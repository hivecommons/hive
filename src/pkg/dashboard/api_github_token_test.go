package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hivecommons/hive#19633: write-only PAT rotation for PAT-mode hives.

const githubTokenPath = "/api/github-token"

type githubTokenFakes struct {
	verified []string
	applied  []string
	verifyFn func(token string) (string, error)
}

func githubTokenServer(t *testing.T) (*Server, *githubTokenFakes) {
	t.Helper()
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	s := covApiServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	s.deps.Config.GitHub.Token = "ghp_old_token"
	f := &githubTokenFakes{verifyFn: func(string) (string, error) { return "rotator", nil }}
	s.deps.VerifyGitHubToken = func(_ context.Context, token string) (string, error) {
		f.verified = append(f.verified, token)
		return f.verifyFn(token)
	}
	s.deps.ApplyGitHubToken = func(token string) { f.applied = append(f.applied, token) }
	return s, f
}

func doGitHubTokenPut(s *Server, body any, owner bool) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, githubTokenPath, &b)
	req.Header.Set("Content-Type", "application/json")
	if owner {
		markOwnerRequest(req)
		req.Header.Set("X-Hive-User", "owner-login")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func githubTokenAudits(s *Server, action string) []string {
	var out []string
	for _, e := range s.audit.Recent(100) {
		if e.Action == action {
			out = append(out, e.Detail)
		}
	}
	return out
}

func TestGitHubToken_RotatesVerifiedTokenLive(t *testing.T) {
	s, f := githubTokenServer(t)

	rec := doGitHubTokenPut(s, map[string]any{"token": "  ghp_new_token\n"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ghp_new_token") || strings.Contains(rec.Body.String(), "ghp_old_token") {
		t.Fatalf("the response must never carry a token: %s", rec.Body.String())
	}
	got := decodeMap(t, rec)
	wantFP := githubTokenFingerprint("ghp_new_token")
	if got["fingerprint"] != wantFP || got["login"] != "rotator" || got["mode"] != "pat" || got["rotatable"] != true {
		t.Fatalf("PUT body = %v, want fingerprint %s and login rotator", got, wantFP)
	}
	if v, _ := got["verified_at"].(string); v == "" {
		t.Fatalf("PUT body has no verified_at: %v", got)
	}
	if len(f.verified) != 1 || f.verified[0] != "ghp_new_token" {
		t.Fatalf("verifier saw %q, want the trimmed new token once", f.verified)
	}
	if len(f.applied) != 1 || f.applied[0] != "ghp_new_token" {
		t.Fatalf("ApplyGitHubToken saw %q, want the new token once", f.applied)
	}
	if s.deps.Config.GitHub.Token != "ghp_new_token" {
		t.Fatalf("live github.token = %q, want the new token", s.deps.Config.GitHub.Token)
	}
	if _, err := os.Stat(s.deps.Config.SourcePath); err != nil {
		t.Fatalf("the rotation was not persisted through the config save path: %v", err)
	}

	audits := githubTokenAudits(s, "github_token_rotated")
	if len(audits) != 1 {
		t.Fatalf("github_token_rotated audits = %v, want 1", audits)
	}
	for _, want := range []string{"fingerprint=" + wantFP, "previous_fingerprint=" + githubTokenFingerprint("ghp_old_token"), "login=rotator"} {
		if !strings.Contains(audits[0], want) {
			t.Fatalf("audit %q does not record %q", audits[0], want)
		}
	}
	if strings.Contains(audits[0], "ghp_new_token") {
		t.Fatalf("audit leaks the token: %q", audits[0])
	}
	if !auditSensitiveActions["github_token_rotated"] || !auditSensitiveActions["github_token_rejected"] {
		t.Fatal("a credential change must count toward the Audit Log risk chip")
	}

	// GET shows the same, still without the token.
	rec = doOwnerGet(s, githubTokenPath)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "ghp_new_token") {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if m := decodeMap(t, rec); m["fingerprint"] != wantFP || m["login"] != "rotator" || m["configured"] != true {
		t.Fatalf("GET body = %v", m)
	}
}

func TestGitHubToken_RejectedTokenChangesNothing(t *testing.T) {
	s, f := githubTokenServer(t)
	f.verifyFn = func(token string) (string, error) {
		return "", errors.New("GitHub rejected the token at GET /user: token invalid: status 401 (" + token + ")")
	}

	rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_bad_token"}, true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT bad token: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ghp_bad_token") {
		t.Fatalf("the error must not echo the token: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "GET /user") {
		t.Fatalf("the error must name the failed check: %s", rec.Body.String())
	}
	if s.deps.Config.GitHub.Token != "ghp_old_token" {
		t.Fatalf("live github.token changed to %q after a refused rotation", s.deps.Config.GitHub.Token)
	}
	if len(f.applied) != 0 {
		t.Fatalf("a refused token was applied: %q", f.applied)
	}
	if _, err := os.Stat(s.deps.Config.SourcePath); !os.IsNotExist(err) {
		t.Fatalf("a refused token reached the config file (stat err %v)", err)
	}
	audits := githubTokenAudits(s, "github_token_rejected")
	if len(audits) != 1 || !strings.Contains(audits[0], "fingerprint="+githubTokenFingerprint("ghp_bad_token")) || strings.Contains(audits[0], "ghp_bad_token") {
		t.Fatalf("github_token_rejected audits = %v", audits)
	}
	if len(githubTokenAudits(s, "github_token_rotated")) != 0 {
		t.Fatal("a refused token was audited as rotated")
	}
}

func TestGitHubToken_SaveFailureRestoresPreviousToken(t *testing.T) {
	s, f := githubTokenServer(t)
	// Config.Save's guard refuses a config without project.org, so the save
	// path fails without touching any file.
	s.deps.Config.Project.Org = ""

	rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with failing save: %d %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.GitHub.Token != "ghp_old_token" {
		t.Fatalf("live github.token = %q after a failed save, want the previous token", s.deps.Config.GitHub.Token)
	}
	if len(f.applied) != 0 {
		t.Fatalf("an unsaved token was applied: %q", f.applied)
	}
}

func TestGitHubToken_RefusedOnAppHiveAndForNonOwners(t *testing.T) {
	s, f := githubTokenServer(t)

	if rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT: expected 403, got %d", rec.Code)
	}
	if rec := doGetNoRole(s, githubTokenPath); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: expected 403, got %d", rec.Code)
	}
	if rec := doGitHubTokenPut(s, map[string]any{"token": "  "}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty token: expected 400, got %d", rec.Code)
	}

	s.deps.Config.GitHub.AppID = 123456
	if rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true); rec.Code != http.StatusConflict {
		t.Fatalf("App hive PUT: expected 409, got %d %s", rec.Code, rec.Body.String())
	}
	if m := decodeMap(t, doOwnerGet(s, githubTokenPath)); m["mode"] != "app" || m["rotatable"] != false {
		t.Fatalf("App hive GET = %v", m)
	}
	if len(f.verified) != 0 || len(f.applied) != 0 || s.deps.Config.GitHub.Token != "ghp_old_token" {
		t.Fatalf("a refused request reached the verifier or changed the token: verified=%q applied=%q", f.verified, f.applied)
	}

	// No wiring: 503, never a panic.
	s.deps.Config.GitHub.AppID = 0
	s.deps.VerifyGitHubToken = nil
	if rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired PUT: expected 503, got %d", rec.Code)
	}
}

func TestGitHubToken_RefusedWithoutSourcePath(t *testing.T) {
	s, f := githubTokenServer(t)
	s.deps.Config.SourcePath = ""

	rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "no source path") {
		t.Fatalf("PUT without a source path: %d %s", rec.Code, rec.Body.String())
	}
	if len(f.verified) != 0 || len(f.applied) != 0 || s.deps.Config.GitHub.Token != "ghp_old_token" {
		t.Fatalf("an in-memory-only rotation went ahead: verified=%q applied=%q token=%q", f.verified, f.applied, s.deps.Config.GitHub.Token)
	}
	if len(githubTokenAudits(s, "github_token_rotated")) != 0 {
		t.Fatal("a token that would be lost on restart was audited as rotated")
	}
}

func TestGitHubToken_AppSetWhileWaitingForLockGets409(t *testing.T) {
	s, f := githubTokenServer(t)

	// Hold the GitHub config lock the way an App setup does, let the PUT
	// queue behind it, then finish the App setup before releasing.
	s.githubConfigMu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true) }()
	time.Sleep(50 * time.Millisecond)
	s.deps.Config.GitHub.AppID = 123456
	s.githubConfigMu.Unlock()

	rec := <-done
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT after app_id was set under the lock: expected 409, got %d %s", rec.Code, rec.Body.String())
	}
	if len(f.verified) != 0 || len(f.applied) != 0 || s.deps.Config.GitHub.Token != "ghp_old_token" {
		t.Fatalf("a PAT write raced an App setup: verified=%q applied=%q token=%q", f.verified, f.applied, s.deps.Config.GitHub.Token)
	}
}

func TestGitHubToken_CSRFGuard(t *testing.T) {
	s, f := githubTokenServer(t)
	put := func(mutate func(*http.Request)) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, githubTokenPath, strings.NewReader(`{"token":"ghp_new_token"}`))
		req.Header.Set("Content-Type", "application/json")
		markOwnerRequest(req)
		req.Header.Set("X-Hive-User", "owner-login")
		mutate(req)
		s.mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := put(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site PUT: expected 403, got %d %s", rec.Code, rec.Body.String())
	}
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		if rec := put(func(r *http.Request) { r.Header.Set("Content-Type", ct) }); rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: expected 415, got %d %s", ct, rec.Code, rec.Body.String())
		}
	}
	if len(f.verified) != 0 || len(f.applied) != 0 {
		t.Fatalf("a refused request reached the verifier or applier: verified=%q applied=%q", f.verified, f.applied)
	}
	// Same-origin and a charset parameter are fine.
	rec := put(func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin JSON PUT: %d %s", rec.Code, rec.Body.String())
	}
}

func TestGitHubToken_UnsealedTokenKeepsConfigFileOwnerOnly(t *testing.T) {
	prev := githubTokenSealable
	t.Cleanup(func() { githubTokenSealable = prev })

	t.Run("unsealed tightens a readable file", func(t *testing.T) {
		githubTokenSealable = func(string) bool { return false }
		s, _ := githubTokenServer(t)
		if err := os.WriteFile(s.deps.Config.SourcePath, []byte("project:\n  org: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
		}
		if m := decodeMap(t, rec); m["sealed"] != false {
			t.Fatalf("response sealed = %v, want false: %v", m["sealed"], m)
		}
		fi, err := os.Stat(s.deps.Config.SourcePath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("config file mode = %v, want no group or world access", fi.Mode().Perm())
		}
		if a := githubTokenAudits(s, "github_token_rotated"); len(a) != 1 || !strings.Contains(a[0], "sealed=false") {
			t.Fatalf("audit = %v, want sealed=false", a)
		}
	})

	t.Run("unsealed creates a missing file owner-only", func(t *testing.T) {
		githubTokenSealable = func(string) bool { return false }
		s, _ := githubTokenServer(t)
		if rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true); rec.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
		}
		fi, err := os.Stat(s.deps.Config.SourcePath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("new config file mode = %v, want 0600", fi.Mode().Perm())
		}
	})

	t.Run("sealed leaves the file mode alone", func(t *testing.T) {
		githubTokenSealable = func(string) bool { return true }
		s, _ := githubTokenServer(t)
		if err := os.WriteFile(s.deps.Config.SourcePath, []byte("project:\n  org: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
		}
		if m := decodeMap(t, rec); m["sealed"] != true {
			t.Fatalf("response sealed = %v, want true: %v", m["sealed"], m)
		}
		fi, _ := os.Stat(s.deps.Config.SourcePath)
		if fi.Mode().Perm() != 0o644 {
			t.Fatalf("config file mode = %v, want it untouched (0644)", fi.Mode().Perm())
		}
	})

	t.Run("a file that cannot be protected refuses the rotation", func(t *testing.T) {
		githubTokenSealable = func(string) bool { return false }
		s, f := githubTokenServer(t)
		// A source path under a regular file: stat fails with ENOTDIR, which
		// is neither "missing" nor "readable", so nothing may be written.
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		s.deps.Config.SourcePath = filepath.Join(blocker, "hive.yaml")
		rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
		}
		if len(f.applied) != 0 || s.deps.Config.GitHub.Token != "ghp_old_token" {
			t.Fatalf("an unprotectable rotation went ahead: applied=%q token=%q", f.applied, s.deps.Config.GitHub.Token)
		}
	})
}

func TestGitHubToken_RejectedAttemptsAreRateLimited(t *testing.T) {
	t.Setenv(githubTokenRejectCooldownEnv, "")
	s, f := githubTokenServer(t)
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	s.githubTokenNow = func() time.Time { return now }
	f.verifyFn = func(string) (string, error) { return "", errors.New("GitHub rejected the token at GET /user") }

	if rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_bad_token"}, true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("first bad PUT: %d %s", rec.Code, rec.Body.String())
	}
	now = now.Add(5 * time.Second)
	rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_other_token"}, true)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("PUT inside the cooldown: expected 429, got %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "10" {
		t.Fatalf("Retry-After = %q, want 10", got)
	}
	if !strings.Contains(rec.Body.String(), "retry in 10 seconds") {
		t.Fatalf("429 body has no retry hint: %s", rec.Body.String())
	}
	if len(f.verified) != 1 {
		t.Fatalf("a throttled attempt reached GitHub: verified=%q", f.verified)
	}

	// After the window a good token goes through.
	now = now.Add(defaultGitHubTokenRejectCooldown)
	f.verifyFn = func(string) (string, error) { return "rotator", nil }
	if rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_new_token"}, true); rec.Code != http.StatusOK {
		t.Fatalf("PUT after the cooldown: %d %s", rec.Code, rec.Body.String())
	}
}

func TestGitHubToken_CooldownIsConfigurable(t *testing.T) {
	t.Setenv(githubTokenRejectCooldownEnv, "0")
	s, f := githubTokenServer(t)
	f.verifyFn = func(string) (string, error) { return "", errors.New("GitHub rejected the token at GET /user") }
	for i := 0; i < 2; i++ {
		if rec := doGitHubTokenPut(s, map[string]any{"token": "ghp_bad_token"}, true); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d with the cooldown off: %d %s", i, rec.Code, rec.Body.String())
		}
	}
}
