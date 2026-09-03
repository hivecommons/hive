package github

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// scopeTestServer returns a client pointed at a test server whose /rate_limit
// response carries the given X-OAuth-Scopes header. setHeader=false omits the
// header entirely (the App-token / non-GitHub shape); setHeader=true with an
// empty value is the fine-grained-PAT shape, which is the case most likely to
// regress into a false alarm.
func scopeTestServer(t *testing.T, setHeader bool, scopes string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if setHeader {
			w.Header().Set(oauthScopesHeader, scopes)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resources":{}}`))
	}))
	t.Cleanup(srv.Close)
	return NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
}

func TestCheckTokenScopes(t *testing.T) {
	tests := []struct {
		name       string
		setHeader  bool
		scopes     string
		level      int
		wantStatus ScopeStatus
		// wantDetailContains asserts the message NAMES the scope and the
		// capability. A generic "insufficient permissions" is the exact failure
		// this feature removes, so an empty-detail pass is not acceptable.
		wantDetailContains []string
		wantMissing        []string
	}{
		{
			// Positive control's counterpart: a full-scope token at the highest
			// level must be silent.
			name:       "sufficient scopes at L6 produce no warning",
			setHeader:  true,
			scopes:     "repo, read:org, workflow",
			level:      6,
			wantStatus: ScopeStatusOK,
		},
		{
			// POSITIVE CONTROL for the case above: same level, narrower token,
			// must warn AND name "repo" plus the PR capability.
			name:               "missing repo at L6 warns naming scope and capability",
			setHeader:          true,
			scopes:             "read:org",
			level:              6,
			wantStatus:         ScopeStatusMissing,
			wantMissing:        []string{scopeRepo},
			wantDetailContains: []string{`"repo"`, "pull requests"},
		},
		{
			// THE MOST IMPORTANT CASE. Fine-grained PATs report no scopes.
			// Reporting "missing" here would spam a false alarm at every boot of
			// every fine-grained-token deployment.
			name:       "empty X-OAuth-Scopes reports undetermined not missing",
			setHeader:  true,
			scopes:     "",
			level:      6,
			wantStatus: ScopeStatusUndetermined,
		},
		{
			name:       "absent X-OAuth-Scopes header reports undetermined",
			setHeader:  false,
			level:      6,
			wantStatus: ScopeStatusUndetermined,
		},
		{
			// ACMM tiering: read-only advisory needs far less than PR creation.
			// public_repo alone is fine at L2 …
			name:       "public_repo alone is sufficient at L2",
			setHeader:  true,
			scopes:     "public_repo",
			level:      2,
			wantStatus: ScopeStatusOK,
		},
		{
			// … and the SAME token warns at L5. This pair is what proves the
			// level actually gates the requirement rather than the check being
			// level-blind.
			name:               "same public_repo token warns at L5",
			setHeader:          true,
			scopes:             "public_repo",
			level:              5,
			wantStatus:         ScopeStatusMissing,
			wantMissing:        []string{scopeRepo, scopeReadOrg},
			wantDetailContains: []string{`"repo"`, `"read:org"`},
		},
		{
			// Scope hierarchy: "repo" implicitly covers public_repo and
			// repo:status, and GitHub reports only the broader scope. Without
			// the alternatives table this would false-alarm.
			name:       "repo subsumes public_repo and repo:status",
			setHeader:  true,
			scopes:     "repo, read:org",
			level:      5,
			wantStatus: ScopeStatusOK,
		},
		{
			// L1 exercises no GitHub capability, so nothing is required.
			name:       "L1 requires no scopes",
			setHeader:  true,
			scopes:     "public_repo",
			level:      1,
			wantStatus: ScopeStatusOK,
		},
		{
			// An unset level must resolve to the MOST capable level, not the
			// least — defaulting low would silently suppress real findings.
			name:        "unset level is evaluated at the highest level",
			setHeader:   true,
			scopes:      "public_repo",
			level:       ACMMLevelUnset,
			wantStatus:  ScopeStatusMissing,
			wantMissing: []string{scopeRepo, scopeReadOrg},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := scopeTestServer(t, tt.setHeader, tt.scopes)
			got := c.CheckTokenScopes(context.Background(), tt.level)

			if got.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q (detail=%q reason=%q)", got.Status, tt.wantStatus, got.Detail, got.Reason)
			}
			for _, want := range tt.wantMissing {
				if !contains(got.Missing, want) {
					t.Errorf("missing scopes %v does not include %q", got.Missing, want)
				}
			}
			for _, sub := range tt.wantDetailContains {
				if !strings.Contains(got.Detail, sub) {
					t.Errorf("detail %q does not contain %q", got.Detail, sub)
				}
			}
			// An OK result must never carry a warning payload — that is what
			// "zero noise when correct" means concretely.
			if got.Status == ScopeStatusOK && (got.Detail != "" || len(got.Missing) > 0) {
				t.Errorf("OK result carried detail=%q missing=%v, want both empty", got.Detail, got.Missing)
			}
			// Undetermined must NEVER assert a missing scope.
			if got.Status == ScopeStatusUndetermined && len(got.Missing) > 0 {
				t.Errorf("undetermined result claimed missing scopes %v — absence of evidence is not evidence of absence", got.Missing)
			}
			if got.Status == ScopeStatusUndetermined && got.Reason == "" {
				t.Error("undetermined result gave no reason, leaving the operator with nothing to act on")
			}
		})
	}
}

// TestCheckTokenScopesSkipsAppAuth asserts the App path is skipped entirely.
// An App has permissions, not scopes; warning about a PAT that is not in use
// would be noise pointing at the wrong thing.
func TestCheckTokenScopesSkipsAppAuth(t *testing.T) {
	c := scopeTestServer(t, true, "") // header says "no scopes" — would be Undetermined on the PAT path
	c.appAuth = &AppAuth{}

	got := c.CheckTokenScopes(context.Background(), 6)
	if got.Status != ScopeStatusSkipped {
		t.Fatalf("status = %q, want %q", got.Status, ScopeStatusSkipped)
	}
	if len(got.Missing) > 0 || got.Detail != "" {
		t.Errorf("App path produced a PAT warning: missing=%v detail=%q", got.Missing, got.Detail)
	}

	// POSITIVE CONTROL: the identical server WITHOUT appAuth must not skip,
	// proving the skip came from App detection and not from the server shape.
	c2 := scopeTestServer(t, true, "")
	if got2 := c2.CheckTokenScopes(context.Background(), 6); got2.Status == ScopeStatusSkipped {
		t.Error("token path also skipped — the test cannot distinguish App auth from any other skip")
	}
}

// TestCheckTokenScopesNetworkFailureIsSoft asserts a dead endpoint yields a
// skip-shaped result and, critically, that execution PROCEEDS — the check must
// never become a new startup failure mode.
func TestCheckTokenScopesNetworkFailureIsSoft(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // closed: connections are refused

	c := NewClientForTest(url, "hivecommons", []string{"hive"}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	proceeded := false
	got := c.CheckTokenScopes(context.Background(), 6)
	proceeded = true // reaching here at all is the assertion: no panic, no exit

	if !proceeded {
		t.Fatal("startup did not proceed past the scope check")
	}
	if got.Status != ScopeStatusUndetermined {
		t.Fatalf("status = %q, want %q", got.Status, ScopeStatusUndetermined)
	}
	if len(got.Missing) > 0 {
		t.Errorf("unreachable GitHub produced missing scopes %v — a network fault must never be reported as a token defect", got.Missing)
	}
}

// TestCheckTokenScopesNilReceiver covers the dashboard-only boot: a hive with
// no usable credentials runs with a nil *Client for the life of the process.
func TestCheckTokenScopesNilReceiver(t *testing.T) {
	var c *Client
	if got := c.CheckTokenScopes(context.Background(), 6); got.Status != ScopeStatusSkipped {
		t.Fatalf("status = %q, want %q", got.Status, ScopeStatusSkipped)
	}
}

// TestLogTokenScopeCheckNeverLogsTokenMaterial is the security assertion: the
// diagnostic may name scopes and capabilities, never credentials.
func TestLogTokenScopeCheckNeverLogsTokenMaterial(t *testing.T) {
	const secret = "ghp_supersecrettokenvalue000000000000"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(oauthScopesHeader, "public_repo")
		_, _ = w.Write([]byte(`{"resources":{}}`))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// NewClientForTest hardcodes a placeholder token, so build the real client
	// with the secret and then retarget its BaseURL at the test server — the
	// point of this test is that a REAL token value never reaches the log.
	c := NewClient(secret, "hivecommons", []string{"hive"}, logger, "")
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse test url: %v", err)
	}
	c.client.BaseURL = base

	res := c.LogTokenScopeCheck(context.Background(), logger, 6)

	// POSITIVE CONTROL: the logger must actually have been exercised, otherwise
	// "no token in the log" would pass trivially on an empty buffer.
	if res.Status != ScopeStatusMissing {
		t.Fatalf("expected a warning to be emitted, got status %q", res.Status)
	}
	out := buf.String()
	if !strings.Contains(out, "repo") {
		t.Fatalf("log did not contain the expected scope warning, so the no-secret assertion is vacuous: %q", out)
	}

	if strings.Contains(out, secret) {
		t.Error("log contained the full token")
	}
	// Prefixes and suffixes are token material too — a 12-char slice of a PAT
	// is still a credential fragment.
	const fragment = 12
	if strings.Contains(out, secret[:fragment]) {
		t.Error("log contained a token prefix")
	}
	if strings.Contains(out, secret[len(secret)-fragment:]) {
		t.Error("log contained a token suffix")
	}
}

// TestLogTokenScopeCheckSilentWhenCorrect pins requirement 5: a correctly
// scoped token produces no output at all, so any line from this check is
// actionable by construction.
func TestLogTokenScopeCheckSilentWhenCorrect(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c := scopeTestServer(t, true, "repo, read:org")
	if res := c.LogTokenScopeCheck(context.Background(), logger, 6); res.Status != ScopeStatusOK {
		t.Fatalf("status = %q, want %q (reason=%q)", res.Status, ScopeStatusOK, res.Reason)
	}
	if buf.Len() != 0 {
		t.Errorf("correctly-scoped token produced log output: %q", buf.String())
	}

	// POSITIVE CONTROL: the same logger DOES emit for a narrow token, proving
	// the silence above is the check's judgement and not a dead logger.
	buf.Reset()
	c2 := scopeTestServer(t, true, "public_repo")
	if res := c2.LogTokenScopeCheck(context.Background(), logger, 6); res.Status != ScopeStatusMissing {
		t.Fatalf("control: status = %q, want %q", res.Status, ScopeStatusMissing)
	}
	if buf.Len() == 0 {
		t.Error("control: narrow token produced no log output, so the silence assertion proves nothing")
	}
}

func TestParseScopeHeader(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{",", nil},
		{", ", nil},
		{"repo", []string{"repo"}},
		{"repo, read:org", []string{"read:org", "repo"}},
		{"  repo ,, read:org  ", []string{"read:org", "repo"}},
	}
	for _, tt := range tests {
		got := parseScopeHeader(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("parseScopeHeader(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseScopeHeader(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
