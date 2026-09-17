package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The point of seat verification (#7309) is that three different failures that
// previously looked identical must now produce three different states. Each
// test below pins one of them, plus the wording rule that makes the state
// actionable — a 403 that is not a licence verdict must NOT tell the operator
// their seat is bad, because on #7302 that was a false claim.

// copilotSeatStub points copilotUserEndpointURL at a stub for the test's
// lifetime, so verification never reaches api.github.com.
func copilotSeatStub(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	prev := copilotUserEndpointURL
	copilotUserEndpointURL = srv.URL
	t.Cleanup(func() { copilotUserEndpointURL = prev })
}

func TestVerifyCopilotSeatActiveOnOK(t *testing.T) {
	copilotSeatStub(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token tok" {
			t.Errorf("Authorization = %q, want the token under verification", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"endpoints": map[string]any{"api": "https://api.githubcopilot.com"}})
	})

	v := verifyCopilotSeat("tok", "the dashboard Copilot login (GitHub account @alice)")

	if v.State != copilotSeatActive {
		t.Fatalf("state = %q, want %q", v.State, copilotSeatActive)
	}
	if v.Credential != "the dashboard Copilot login (GitHub account @alice)" {
		t.Fatalf("credential = %q; a verdict must name what it verified", v.Credential)
	}
	if v.CheckedAt.IsZero() {
		t.Fatal("CheckedAt must be set on a completed check")
	}
}

// A licence verdict is the only state that may tell the operator to go look at
// their seat.
func TestVerifyCopilotSeatNoSeatOnLicenceVerdict(t *testing.T) {
	copilotSeatStub(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized: not licensed to use Copilot", http.StatusForbidden)
	})

	v := verifyCopilotSeat("tok", "the dashboard Copilot login (GitHub account @alice)")

	if v.State != copilotSeatNoSeat {
		t.Fatalf("state = %q, want %q", v.State, copilotSeatNoSeat)
	}
	if !strings.Contains(v.Detail, "github.com/settings/copilot") {
		t.Fatalf("a no-seat verdict must point at the seat page; detail = %q", v.Detail)
	}
}

// Cause 2 on #7302: the seat is fine, an org policy refuses the integration.
// Telling this operator to check their seat is the exact false claim that made
// #7302 take two round trips.
func TestVerifyCopilotSeatBlockedOnNonLicence403(t *testing.T) {
	copilotSeatStub(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"access denied by policy"}`, http.StatusForbidden)
	})

	v := verifyCopilotSeat("tok", "the COPILOT_GITHUB_TOKEN environment variable")

	if v.State != copilotSeatBlocked {
		t.Fatalf("state = %q, want %q", v.State, copilotSeatBlocked)
	}
	if !strings.Contains(v.Detail, "HIVE_COPILOT_INTEGRATION_ID") {
		t.Fatalf("a policy verdict must name the integration-ID escape hatch; detail = %q", v.Detail)
	}
	if strings.Contains(strings.ToLower(v.Detail), "no copilot seat") {
		t.Fatalf("a non-licence 403 must not claim the seat is bad; detail = %q", v.Detail)
	}
}

// Cause 1 on #7302: a token was saved but never activated server-side.
func TestVerifyCopilotSeatRejectedOn401(t *testing.T) {
	copilotSeatStub(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Bad credentials", http.StatusUnauthorized)
	})

	v := verifyCopilotSeat("tok", "the dashboard Copilot login")

	if v.State != copilotSeatRejected {
		t.Fatalf("state = %q, want %q", v.State, copilotSeatRejected)
	}
	if strings.Contains(strings.ToLower(v.Detail), "seat") &&
		!strings.Contains(strings.ToLower(v.Detail), "activate") {
		t.Fatalf("a 401 is an activation failure, not a seat verdict; detail = %q", v.Detail)
	}
}

// Transient and hive-side failures must never be reported as account problems:
// that is what trains owners to ignore the label.
func TestVerifyCopilotSeatUnreachableIsNotAnAccountVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "server error", status: http.StatusInternalServerError},
		{name: "bad gateway", status: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copilotSeatStub(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", tc.status)
			})

			v := verifyCopilotSeat("tok", "the dashboard Copilot login")

			if v.State != copilotSeatUnreachable {
				t.Fatalf("state = %q, want %q", v.State, copilotSeatUnreachable)
			}
			if strings.Contains(strings.ToLower(v.Detail), "not licensed") {
				t.Fatalf("a transient failure must not read as a licence verdict; detail = %q", v.Detail)
			}
		})
	}
}

func TestVerifyCopilotSeatUnreachableWhenGitHubIsDown(t *testing.T) {
	prev := copilotUserEndpointURL
	// A port nothing listens on: Do() fails before any status exists.
	copilotUserEndpointURL = "http://127.0.0.1:1"
	t.Cleanup(func() { copilotUserEndpointURL = prev })

	v := verifyCopilotSeat("tok", "the dashboard Copilot login")

	if v.State != copilotSeatUnreachable {
		t.Fatalf("state = %q, want %q", v.State, copilotSeatUnreachable)
	}
}

// No credential is "not configured", never a failure verdict.
func TestVerifyCopilotSeatUnknownWithoutToken(t *testing.T) {
	copilotSeatStub(t, func(http.ResponseWriter, *http.Request) {
		t.Error("verification must not call GitHub when there is no token")
	})

	for _, token := range []string{"", "   "} {
		v := verifyCopilotSeat(token, "unused")
		if v.State != copilotSeatUnknown {
			t.Fatalf("state for token %q = %q, want %q", token, v.State, copilotSeatUnknown)
		}
		if v.Detail != "" {
			t.Fatalf("an unknown verdict must carry no failure detail, got %q", v.Detail)
		}
	}
}

// The cache exists so the polled status endpoint costs no network calls. If it
// ever returned a verdict for a DIFFERENT token, the dashboard would report a
// stale account's seat after a re-login.
func TestCopilotSeatCacheIsKeyedByToken(t *testing.T) {
	var c copilotSeatCache
	c.put("tok-a", copilotSeatVerdict{State: copilotSeatActive})

	if v, ok := c.get("tok-a"); !ok || v.State != copilotSeatActive {
		t.Fatalf("get(tok-a) = %#v, %v; want the stored verdict", v, ok)
	}
	if _, ok := c.get("tok-b"); ok {
		t.Fatal("a verdict for one token must never be served for another")
	}
	if _, ok := c.get(""); ok {
		t.Fatal("the empty token must never hit the cache")
	}

	c.put("tok-b", copilotSeatVerdict{State: copilotSeatNoSeat})
	if _, ok := c.get("tok-a"); ok {
		t.Fatal("a new token must displace the previous verdict")
	}
}

// copilotSeatStatus(false) is what the polled endpoint calls; it must be free.
func TestCopilotSeatStatusDoesNotProbeWithoutForce(t *testing.T) {
	copilotSeatStub(t, func(http.ResponseWriter, *http.Request) {
		t.Error("the polled status path must never make a network call")
	})
	t.Setenv("COPILOT_GITHUB_TOKEN", "tok-env")
	s := &Server{}

	if v := s.copilotSeatStatus(false); v.State != copilotSeatUnknown {
		t.Fatalf("uncached status = %q, want %q", v.State, copilotSeatUnknown)
	}
}

func TestCopilotSeatStatusForcesProbeAndCaches(t *testing.T) {
	var calls int
	copilotSeatStub(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"endpoints": map[string]any{"api": "x"}})
	})
	prevLookup := lookupCopilotTokenLogin
	lookupCopilotTokenLogin = func(string) string { return "alice" }
	t.Cleanup(func() { lookupCopilotTokenLogin = prevLookup })
	t.Setenv("COPILOT_GITHUB_TOKEN", "tok-env")
	s := &Server{}

	if v := s.copilotSeatStatus(true); v.State != copilotSeatActive {
		t.Fatalf("forced status = %q, want %q", v.State, copilotSeatActive)
	}
	// The forced result must now be readable without another probe.
	v := s.copilotSeatStatus(false)
	if v.State != copilotSeatActive {
		t.Fatalf("cached status = %q, want %q", v.State, copilotSeatActive)
	}
	if calls != 1 {
		t.Fatalf("probe count = %d, want 1: the cached read must not re-probe", calls)
	}
	if !strings.Contains(v.Credential, "alice") {
		t.Fatalf("credential = %q; the verdict must name the resolved account", v.Credential)
	}
}

func TestCopilotSeatStatusUnknownWithoutCredential(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	s := &Server{}

	if v := s.copilotSeatStatus(true); v.State != copilotSeatUnknown {
		t.Fatalf("state = %q, want %q when hive holds no Copilot credential", v.State, copilotSeatUnknown)
	}
}

// The status endpoint is the adopter's only view of this. logged_in alone was
// the bug; the payload must carry the seat verdict alongside it.
func TestCopilotAuthStatusPayloadCarriesSeatVerdict(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	s := NewServer(0, nil)
	rec := httptest.NewRecorder()

	s.handleCopilotAuthStatus(rec, httptest.NewRequest(http.MethodGet, "/api/copilot-auth/status", nil))

	var got struct {
		LoggedIn bool               `json:"logged_in"`
		Seat     copilotSeatVerdict `json:"seat"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding status payload: %v (body %s)", err, rec.Body)
	}
	if got.Seat.State != copilotSeatUnknown {
		t.Fatalf("seat.state = %q, want %q", got.Seat.State, copilotSeatUnknown)
	}
}

func TestItoaStatusRendersCodes(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{{200, "200"}, {503, "503"}, {7, "7"}, {0, "0"}, {-1, "0"}} {
		if got := itoaStatus(tc.in); got != tc.want {
			t.Fatalf("itoaStatus(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
