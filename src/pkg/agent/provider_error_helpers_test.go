package agent

import "testing"

// classifyProviderError branches for overloaded errors, contextual
// rate-limit prose, and bare HTTP statuses with API context.
func TestClassifyProviderError_StatusAndOverloadBranches(t *testing.T) {
	cases := []struct {
		name      string
		pane      string
		wantClass string
		want      bool
	}{
		{
			name:      "overloaded_error json",
			pane:      `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantClass: "overloaded",
			want:      true,
		},
		{
			name:      "overloaded prose with api context",
			pane:      `inference backend overloaded, shedding load`,
			wantClass: "overloaded",
			want:      true,
		},
		{
			name:      "too many requests with api context",
			pane:      `inference backend: too many requests, slow down`,
			wantClass: "rate_limit",
			want:      true,
		},
		{
			name:      "rate limit prose with api context",
			pane:      `API error: rate limit reached for model`,
			wantClass: "rate_limit",
			want:      true,
		},
		{
			name:      "api error status 529",
			pane:      `API Error: 529 upstream saturated`,
			wantClass: "overloaded",
			want:      true,
		},
		{
			name:      "api error status 429",
			pane:      `API Error: 429 slow down`,
			wantClass: "rate_limit",
			want:      true,
		},
		{
			name:      "http status with api context",
			pane:      `inference backend returned 503`,
			wantClass: "api_error",
			want:      true,
		},
		{
			name:      "http auth status with api context",
			pane:      `backend rejected request: 401`,
			wantClass: "auth",
			want:      true,
		},
		{
			name:      "copilot seat revoked renders without a status code (#6500)",
			pane:      `✗ You are not licensed to use Copilot. (Request ID: CF24:249477:13194BA:1505534:6AA2A42E)`,
			wantClass: "auth",
			want:      true,
		},
		{
			name: "prose about licensing is not an error",
			pane: "docs: explain who is licensed to use Copilot in the org",
			want: false,
		},
		{
			name: "bare status without api context is not an error",
			pane: "503 lines changed in the diff",
			want: false,
		},
		{
			name: "rate limit prose without api context is not an error",
			pane: "the function should rate limit outbound emails",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyProviderError(tc.pane)
			if ok != tc.want {
				t.Fatalf("classifyProviderError(%q) ok=%v, want %v (match=%+v)", tc.pane, ok, tc.want, got)
			}
			if tc.want && got.Class != tc.wantClass {
				t.Fatalf("classifyProviderError(%q) class=%q, want %q", tc.pane, got.Class, tc.wantClass)
			}
		})
	}
}

// paneAfterKickBaseline trims pre-kick scrollback so stale provider errors are
// not re-detected. The line-prefix fallback and disjoint branches were
// previously uncovered.

// clearProviderErrorLocked resets backoff state. The early-return and the
// LastError-preservation branches were previously uncovered.
