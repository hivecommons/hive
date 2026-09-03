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
		AppStateNoAppAssigned, AppStateWriteForbidden, AppStateRepoNotCovered,
		AppStateRepoMoved,
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
		AppStateNoAppAssigned, AppStateWriteForbidden, AppStateRepoNotCovered,
		AppStateRepoMoved,
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

// TestAppStateWriteForbidden_MessageIsAccurate (#2353) locks in the honest copy
// for a write-403 on a healthy installation: it must NOT claim the App lacks
// Issues: Read & Write (the permission IS granted), must name the repo, and must
// point at the real likely cause — the repo not being in the installation's
// selected repositories. It is user-actionable (an org owner adds the repo),
// never operator-side.
func TestAppStateWriteForbidden_MessageIsAccurate(t *testing.T) {
	d := AppAuthDiagnosis{
		State:           AppStateWriteForbidden,
		ExpectedAccount: "open-horizon-services",
		Repo:            "Getting-Started",
	}
	msg := d.Message()
	if msg == "" {
		t.Fatal("write-forbidden must carry banner copy")
	}
	if strings.Contains(msg, "lacks Issues") || strings.Contains(msg, "granted Issues: none") {
		t.Errorf("must not claim a permission gap; got %q", msg)
	}
	if !strings.Contains(msg, "Getting-Started") {
		t.Errorf("message must name the repo; got %q", msg)
	}
	if !strings.Contains(msg, "selected repositories") {
		t.Errorf("message must point at repo-scope as the likely cause; got %q", msg)
	}
	if AppStateWriteForbidden.OperatorActionable() {
		t.Error("write-forbidden is fixed by an org owner, not the operator")
	}
	if !AppStateWriteForbidden.UserActionable() {
		t.Error("write-forbidden must be user-actionable (add the repo to the installation)")
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

// TestDiagnoseAppAuth_RecordsVisualHiveExecutionGrants is the #4030 gap: before
// this, DiagnoseAppAuth read only the issues permission, so the Actions and
// Commit-statuses grants were not merely unenforced — they were unobservable.
// Every installation reported "ok" whether or not those grants had landed,
// which is what would make a half-approved fleet invisible.
//
// The table's discriminating pair is "ordinary Hive App" vs "Hive App with the
// Visual Hive grants": both are AppStateOK, and only ExecutionGrants /
// GrantsVisualHiveExecution tell them apart.
func TestDiagnoseAppAuth_RecordsVisualHiveExecutionGrants(t *testing.T) {
	tests := []struct {
		name         string
		permissions  string
		wantState    AppAuthState
		wantGrants   string
		wantSatisfie bool
	}{
		{
			// The ~55 installations that will never use Visual Hive. Healthy,
			// and must stay healthy — see the invariant test below.
			name:         "ordinary Hive App grants neither",
			permissions:  `{"issues":"write"}`,
			wantState:    AppStateOK,
			wantGrants:   "actions=none statuses=none",
			wantSatisfie: false,
		},
		{
			// GitHub reports read for an App that requested read. Still not
			// the write pair Visual Hive needs.
			name:         "read-only Actions and statuses do not satisfy",
			permissions:  `{"issues":"write","actions":"read","statuses":"read"}`,
			wantState:    AppStateOK,
			wantGrants:   "actions=read statuses=read",
			wantSatisfie: false,
		},
		{
			// The half-approved shape: an org owner accepted a widening that
			// only partly landed. Indistinguishable from healthy before #4030.
			name:         "one of the two grants is not enough",
			permissions:  `{"issues":"write","actions":"write","statuses":"read"}`,
			wantState:    AppStateOK,
			wantGrants:   "actions=write statuses=read",
			wantSatisfie: false,
		},
		{
			name:         "both grants at write satisfies",
			permissions:  `{"issues":"write","actions":"write","statuses":"write"}`,
			wantState:    AppStateOK,
			wantGrants:   "actions=write statuses=write",
			wantSatisfie: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":42980,"account":{"login":"katamari"},"permissions":` + tt.permissions + `}`))
			}))
			defer srv.Close()

			got := testAuth(t, srv.URL).DiagnoseAppAuth(context.Background(), "katamari")

			if got.State != tt.wantState {
				t.Fatalf("State = %s, want %s", got.State, tt.wantState)
			}
			if got.ExecutionGrants() != tt.wantGrants {
				t.Errorf("ExecutionGrants() = %q, want %q", got.ExecutionGrants(), tt.wantGrants)
			}
			if got.GrantsVisualHiveExecution() != tt.wantSatisfie {
				t.Errorf("GrantsVisualHiveExecution() = %v, want %v",
					got.GrantsVisualHiveExecution(), tt.wantSatisfie)
			}
		})
	}
}

// TestDiagnoseAppAuth_VisualHiveGrantsAreRecordedNotRequired pins the invariant
// that #4030 turns on: recording the two grants must NOT make them requirements
// of the Hive App.
//
// This is the guard against the tempting wrong fix. Adding "actions" and
// "statuses" to the required set would flip every ordinary installation to
// AppStateInsufficientPerms and raise a banner accusing org owners of a
// permission gap that is deliberate — the exact fleet-wide re-approval event
// the separate Visual Hive App exists to avoid. Remove the separation and the
// first subtest fails.
//
// The second subtest is its mirror: the Visual Hive App's own permission shape
// (Actions and Commit statuses at write, no issues) must NOT be mistaken for a
// healthy Hive App just because the new fields are populated. Classification
// still turns solely on issues.
func TestDiagnoseAppAuth_VisualHiveGrantsAreRecordedNotRequired(t *testing.T) {
	diagnose := func(t *testing.T, permissions string) AppAuthDiagnosis {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42980,"account":{"login":"katamari"},"permissions":` + permissions + `}`))
		}))
		defer srv.Close()
		return testAuth(t, srv.URL).DiagnoseAppAuth(context.Background(), "katamari")
	}

	t.Run("missing Actions and statuses is not a permission fault", func(t *testing.T) {
		got := diagnose(t, `{"issues":"write"}`)
		if got.State != AppStateOK {
			t.Fatalf("State = %s, want ok — the Hive App deliberately lacks these grants "+
				"and must never be accused of a permission gap for it", got.State)
		}
		if got.State.UserActionable() {
			t.Error("an ordinary Hive installation must not be told to fix anything")
		}
		if msg := got.Message(); msg != "" {
			t.Errorf("Message() = %q, want empty for a healthy installation", msg)
		}
	})

	t.Run("Visual Hive grants do not substitute for issues:write", func(t *testing.T) {
		got := diagnose(t, `{"actions":"write","statuses":"write","metadata":"read"}`)
		if got.State != AppStateInsufficientPerms {
			t.Fatalf("State = %s, want insufficient-permissions — classification still "+
				"turns solely on issues", got.State)
		}
		// The grants are still recorded for the very installation the
		// classification rejects; observability is independent of the verdict.
		if !got.GrantsVisualHiveExecution() {
			t.Error("GrantsVisualHiveExecution() = false, want true — the grants are " +
				"recorded regardless of the issues verdict")
		}
	})
}

// TestExecutionGrants_RendersUngrantedAsNone keeps the log token stable. An
// empty string in a log line is ambiguous against a field that was never
// populated, and this rendering is the fleet-countable signal.
func TestExecutionGrants_RendersUngrantedAsNone(t *testing.T) {
	if got := (AppAuthDiagnosis{}).ExecutionGrants(); got != "actions=none statuses=none" {
		t.Errorf("ExecutionGrants() = %q, want %q", got, "actions=none statuses=none")
	}
	if got := (AppAuthDiagnosis{ActionsPerm: "write"}).ExecutionGrants(); got != "actions=write statuses=none" {
		t.Errorf("ExecutionGrants() = %q, want %q", got, "actions=write statuses=none")
	}
	if (AppAuthDiagnosis{}).GrantsVisualHiveExecution() {
		t.Error("a zero diagnosis must not report the Visual Hive grants as satisfied")
	}
}
