package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKeyBits is deliberately small — these tests only need a signable key,
// never a secure one, and 2048-bit generation dominates the runtime.
const testKeyBits = 2048

func appStateTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testAuth(t *testing.T, apiURL string) *AppAuth {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, testKeyBits)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return &AppAuth{appID: 5686, installationID: 42980, key: key, logger: appStateTestLogger(), apiURL: apiURL}
}

// TestDiagnoseAppAuth_Classification is the core table: every failure mode the
// banner copy depends on, driven by the ACTUAL HTTP status the API returns.
func TestDiagnoseAppAuth_Classification(t *testing.T) {
	tests := []struct {
		name string
		// status/body describe the canned GET /app/installations/{id} reply.
		status int
		body   string
		// expectedOwner is the org the hive targets.
		expectedOwner string
		want          AppAuthState
		wantOperator  bool
		wantUser      bool
		// wantMsgHas / wantMsgLacks assert the copy says the true thing.
		wantMsgHas   []string
		wantMsgLacks []string
	}{
		{
			name:          "401 JWT undecodable is an operator key problem",
			status:        http.StatusUnauthorized,
			body:          `{"message":"A JSON web token could not be decoded","documentation_url":"https://docs.github.com/rest"}`,
			expectedOwner: "katamari",
			want:          AppStateKeyInvalid,
			wantOperator:  true,
			wantMsgHas:    []string{"hub operator", "administrator", "not at fault"},
			// Must never send a user who did everything right back to redo it.
			wantMsgLacks: []string{"Install it", "installation_id", "Check github"},
		},
		{
			name:          "403 not accessible is a wrong installation",
			status:        http.StatusForbidden,
			body:          `{"message":"Resource not accessible by integration"}`,
			expectedOwner: "katamari",
			want:          AppStateWrongInstallation,
			wantUser:      true,
			wantMsgHas:    []string{"installation_id"},
		},
		{
			name:          "404 means the app is not installed",
			status:        http.StatusNotFound,
			body:          `{"message":"Not Found"}`,
			expectedOwner: "katamari",
			want:          AppStateNotInstalled,
			wantUser:      true,
			wantMsgHas:    []string{"not installed", "katamari"},
		},
		{
			name:          "installation belonging to another account",
			status:        http.StatusOK,
			body:          `{"id":42980,"account":{"login":"someone-else"},"permissions":{"issues":"write"}}`,
			expectedOwner: "katamari",
			want:          AppStateWrongInstallation,
			wantUser:      true,
			wantMsgHas:    []string{"someone-else", "katamari", "installation_id"},
			wantMsgLacks:  []string{"administrator"},
		},
		{
			name:          "installed on the right org but issues perm not granted",
			status:        http.StatusOK,
			body:          `{"id":42980,"account":{"login":"katamari"},"permissions":{"issues":"read"}}`,
			expectedOwner: "katamari",
			want:          AppStateInsufficientPerms,
			wantUser:      true,
			wantMsgHas:    []string{"Issues: read", "org owner"},
		},
		{
			name:          "success",
			status:        http.StatusOK,
			body:          `{"id":42980,"account":{"login":"katamari"},"permissions":{"issues":"write"}}`,
			expectedOwner: "katamari",
			want:          AppStateOK,
			wantMsgHas:    nil,
		},
		{
			name:          "case-insensitive account match still succeeds",
			status:        http.StatusOK,
			body:          `{"id":42980,"account":{"login":"KataMari"},"permissions":{"issues":"write"}}`,
			expectedOwner: "katamari",
			want:          AppStateOK,
		},
		{
			name:          "500 yields unknown, never an accusation",
			status:        http.StatusInternalServerError,
			body:          `{"message":"Server Error"}`,
			expectedOwner: "katamari",
			want:          AppStateUnknown,
			wantMsgHas:    []string{"No action is needed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			auth := testAuth(t, srv.URL)
			got := auth.DiagnoseAppAuth(context.Background(), tt.expectedOwner)

			if got.State != tt.want {
				t.Fatalf("State = %v (%s), want %v (%s)", got.State, got.State, tt.want, tt.want)
			}
			if got.State.OperatorActionable() != tt.wantOperator {
				t.Errorf("OperatorActionable() = %v, want %v", got.State.OperatorActionable(), tt.wantOperator)
			}
			if got.State.UserActionable() != tt.wantUser {
				t.Errorf("UserActionable() = %v, want %v", got.State.UserActionable(), tt.wantUser)
			}

			msg := got.Message()
			if tt.want == AppStateOK && msg != "" {
				t.Errorf("Message() = %q, want empty for the success state", msg)
			}
			for _, want := range tt.wantMsgHas {
				if !strings.Contains(msg, want) {
					t.Errorf("Message() = %q, want it to contain %q", msg, want)
				}
			}
			for _, lack := range tt.wantMsgLacks {
				if strings.Contains(msg, lack) {
					t.Errorf("Message() = %q, must NOT contain %q", msg, lack)
				}
			}
		})
	}
}

// TestDiagnoseAppAuth_MissingKeyFileSkipsAPI proves the cheap pre-flight: when
// no candidate key path holds content we conclude AppStateKeyMissing without
// ever touching the API.
func TestDiagnoseAppAuth_MissingKeyFileSkipsAPI(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.pem")
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("writing empty key: %v", err)
	}

	auth := testAuth(t, srv.URL)
	got := auth.DiagnoseAppAuth(context.Background(), "katamari", missing, empty)

	if got.State != AppStateKeyMissing {
		t.Fatalf("State = %s, want key-missing", got.State)
	}
	if called {
		t.Error("made an API call despite no readable key file — the pre-flight must short-circuit")
	}
	if !got.State.OperatorActionable() {
		t.Error("key-missing must be operator-actionable")
	}
	msg := got.Message()
	for _, lack := range []string{"Install", "installation_id"} {
		if strings.Contains(msg, lack) {
			t.Errorf("Message() = %q, must not tell the user to %q", msg, lack)
		}
	}
	if !strings.Contains(msg, "hub administrator") {
		t.Errorf("Message() = %q, want it to point at the hub administrator", msg)
	}
}

// TestDiagnoseAppAuth_ReadableKeyStillCallsAPI is the counterpart: a present
// key must not short-circuit, or a genuinely-uninstalled App would be misread
// as a credential problem.
func TestDiagnoseAppAuth_ReadableKeyStillCallsAPI(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	present := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(present, []byte("-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	auth := testAuth(t, srv.URL)
	got := auth.DiagnoseAppAuth(context.Background(), "katamari", present)

	if !called {
		t.Error("did not call the API despite a readable key file")
	}
	if got.State != AppStateNotInstalled {
		t.Fatalf("State = %s, want not-installed", got.State)
	}
}

// TestDiagnoseAppAuth_NilAndKeylessAreOperatorSide guards the two degenerate
// receivers: neither may produce a user-blaming warning.
func TestDiagnoseAppAuth_NilAndKeylessAreOperatorSide(t *testing.T) {
	var nilAuth *AppAuth
	if got := nilAuth.DiagnoseAppAuth(context.Background(), "katamari"); got.State != AppStateKeyMissing {
		t.Errorf("nil receiver State = %s, want key-missing", got.State)
	}
	keyless := &AppAuth{appID: 1, installationID: 2, logger: appStateTestLogger()}
	if got := keyless.DiagnoseAppAuth(context.Background(), "katamari"); got.State != AppStateKeyMissing {
		t.Errorf("keyless State = %s, want key-missing", got.State)
	}
}

// TestAppAuthState_WireRoundTrip locks the tokens that cross the heartbeat.
func TestAppAuthState_WireRoundTrip(t *testing.T) {
	all := []AppAuthState{
		AppStateUnknown, AppStateOK, AppStateNotInstalled, AppStateWrongInstallation,
		AppStateInsufficientPerms, AppStateKeyMissing, AppStateKeyInvalid,
	}
	for _, s := range all {
		if got := ParseAppAuthState(s.String()); got != s {
			t.Errorf("ParseAppAuthState(%q) = %v, want %v", s.String(), got, s)
		}
	}
	// An unrecognised token must degrade to unknown, never to an accusation.
	for _, bogus := range []string{"", "  ", "not-a-state", "NOT-INSTALLED"} {
		if got := ParseAppAuthState(bogus); got != AppStateUnknown {
			t.Errorf("ParseAppAuthState(%q) = %v, want unknown", bogus, got)
		}
	}
}

// TestAppAuthState_ActionabilityIsExclusive asserts no state is ever claimed by
// both sides — the journey nudges branch on exactly this.
func TestAppAuthState_ActionabilityIsExclusive(t *testing.T) {
	all := []AppAuthState{
		AppStateUnknown, AppStateOK, AppStateNotInstalled, AppStateWrongInstallation,
		AppStateInsufficientPerms, AppStateKeyMissing, AppStateKeyInvalid,
	}
	for _, s := range all {
		if s.OperatorActionable() && s.UserActionable() {
			t.Errorf("state %s claims to be both operator- and user-actionable", s)
		}
	}
	if AppStateOK.OperatorActionable() || AppStateOK.UserActionable() {
		t.Error("the OK state must be neither operator- nor user-actionable")
	}
	if AppStateUnknown.OperatorActionable() || AppStateUnknown.UserActionable() {
		t.Error("the unknown state must not be actionable by anyone")
	}
}

// TestClassifyAPIError_RateLimitIsNotACredentialProblem — a 403 rate limit is
// transient and must never latch a credential warning.
func TestClassifyAPIError_RateLimitIsNotACredentialProblem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message":           "API rate limit exceeded",
			"documentation_url": "https://docs.github.com/rest",
		})
	}))
	defer srv.Close()

	auth := testAuth(t, srv.URL)
	got := auth.DiagnoseAppAuth(context.Background(), "katamari")
	if got.State != AppStateUnknown {
		t.Errorf("State = %s, want unknown for a rate-limited 403", got.State)
	}
}

// TestClassifyAPIError_NoResponseIsUnknown — a transport failure yields no
// status, and we must not guess.
func TestClassifyAPIError_NoResponseIsUnknown(t *testing.T) {
	// Port 1 is reserved and refuses connections.
	auth := testAuth(t, "http://127.0.0.1:1")
	got := auth.DiagnoseAppAuth(context.Background(), "katamari")
	if got.State != AppStateUnknown {
		t.Errorf("State = %s, want unknown when GitHub never answered", got.State)
	}
	if got.State.OperatorActionable() || got.State.UserActionable() {
		t.Error("an unreachable API must not be reported as anyone's fault")
	}
}
