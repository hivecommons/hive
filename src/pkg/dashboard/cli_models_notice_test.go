package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Copilot model discovery is best-effort: every failure degrades to a static
// list so a dropdown is never empty. That is right for a transient blip and
// wrong for an ENTITLEMENT verdict — on the kubestellar hive (#6500) GitHub
// answered the catalog probe with 403 "unauthorized: not licensed to use
// Copilot", hive logged it at Info and served twenty static Copilot models, so
// an owner debugging dead agents saw a healthy-looking picker. These tests
// hold the line that a rejection is carried to the dashboard while every other
// failure stays quiet.

// TestCopilotProbeNotice_LicenceRejection covers the exact upstream phrase,
// on both surfaces it can arrive by: the HTTP probe's status+body, and the SDK
// helper forwarding the CLI's JSON error object.
func TestCopilotProbeNotice_LicenceRejection(t *testing.T) {
	for name, err := range map[string]error{
		"http probe body": errors.New("upstream returned 403 (unauthorized: not licensed to use Copilot)"),
		"sdk helper stderr": errors.New(`sdk helper: exit status 1 (stderr: copilot-models: Request models.list failed ` +
			`{"kind":"http","status":403,"statusText":"Forbidden","body":"unauthorized: not licensed to use Copilot\n"})`),
		"mixed case": errors.New("upstream returned 403 (Unauthorized: Not Licensed To Use Copilot)"),
	} {
		t.Run(name, func(t *testing.T) {
			n := copilotProbeNotice(err)
			if n == nil {
				t.Fatalf("no notice for a licence rejection: %v", err)
			}
			if n.Class != "auth" {
				t.Errorf("Class = %q, want auth", n.Class)
			}
			// The label rides into every dropdown option, so it has to name
			// the actual problem rather than repeat "unverified".
			if !strings.Contains(strings.ToLower(n.Label), "licen") {
				t.Errorf("Label = %q, want it to name the licence", n.Label)
			}
			if !strings.Contains(n.Detail, "github.com/settings/copilot") {
				t.Errorf("Detail = %q, want it to point at the seat page", n.Detail)
			}
		})
	}
}

// TestCopilotProbeNotice_BareUnauthorized covers a 401/403 with no explanatory
// body: still an account verdict, but worded as a credential problem rather
// than asserting a licence state upstream did not state.
func TestCopilotProbeNotice_BareUnauthorized(t *testing.T) {
	for _, raw := range []string{
		"upstream returned 401 ",
		"upstream returned 403 ",
		`sdk helper: exit status 1 (stderr: {"kind":"http","status":403})`,
		`sdk helper: exit status 1 (stderr: {"kind": "http", "status": 401})`,
	} {
		n := copilotProbeNotice(errors.New(raw))
		if n == nil {
			t.Fatalf("no notice for %q", raw)
		}
		if n.Class != "auth" {
			t.Errorf("%q: Class = %q, want auth", raw, n.Class)
		}
		if strings.Contains(strings.ToLower(n.Label), "not licensed") {
			t.Errorf("%q: Label = %q asserts a licence state upstream never stated", raw, n.Label)
		}
	}
}

// TestCopilotProbeNotice_QuietForNonAccountFailures is the other half of the
// contract. A probe that could not run, or an upstream that was merely
// unreachable, says nothing about the account — labelling those would train
// owners to ignore the label, which would put us back where #6500 started.
func TestCopilotProbeNotice_QuietForNonAccountFailures(t *testing.T) {
	for _, raw := range []string{
		"sdk helper not installed: stat /usr/local/bin/copilot-models.mjs: no such file or directory",
		"sdk helper: exit status 1 (stderr: copilot-models: timed out after 15000ms)",
		"request failed: dial tcp: lookup api.githubcopilot.com: no such host",
		"upstream returned 500 (internal server error)",
		"upstream returned 429 (rate limited)",
		// A request id or model name containing the digits must not be read
		// as a status code.
		"upstream returned 500 (Request ID: CF24:249477:403194BA:1505534:6AA2A42E)",
		"parse sdk helper output: unexpected end of JSON input",
	} {
		if n := copilotProbeNotice(errors.New(raw)); n != nil {
			t.Errorf("%q produced notice %+v; want none", raw, n)
		}
	}
	if n := copilotProbeNotice(nil); n != nil {
		t.Errorf("nil error produced notice %+v", n)
	}
}

// TestDiscoverCopilotModels_LicenceRejectionCarriesNotice is the end-to-end
// shape of the bug: the SDK probe is rejected, the list served is the static
// fallback, and the result now says WHY instead of looking like an ordinary
// probe miss.
func TestDiscoverCopilotModels_LicenceRejectionCarriesNotice(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return nil, errors.New(`sdk helper: exit status 1 (stderr: copilot-models: Request models.list failed ` +
			`{"kind":"http","status":403,"body":"unauthorized: not licensed to use Copilot\n"})`)
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}

	r := s.discoverCopilotModels()
	if !r.fallback {
		t.Fatalf("a rejected probe must still fall back, got %+v", r)
	}
	if r.notice == nil || r.notice.Class != "auth" {
		t.Fatalf("notice = %+v, want an auth notice", r.notice)
	}

	// The full pipeline still serves a usable, non-empty list — blanking the
	// dropdown would leave the owner unable to configure anything, and a seat
	// can come back — and it carries the notice through the fallback
	// substitution and the cache.
	q := s.queryCLIModels("copilot")
	if len(q.models) == 0 {
		t.Fatal("pipeline served an empty list; the dropdown must never blank")
	}
	if q.notice == nil || q.notice.Class != "auth" {
		t.Fatalf("notice lost in queryCLIModels: %+v", q.notice)
	}
	cached := s.queryCLIModels("copilot")
	if cached.notice == nil {
		t.Fatal("notice lost on the cached read")
	}
}

// TestDiscoverCopilotModels_NoTokenIsNotALicenceProblem guards the most
// damaging possible misfire: telling an owner who simply has not logged in
// yet to go argue with GitHub about a seat they never had.
func TestDiscoverCopilotModels_NoTokenIsNotALicenceProblem(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return nil, errors.New("sdk helper not installed: no such file or directory")
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}

	r := s.discoverCopilotModels()
	if !r.fallback {
		t.Fatalf("no token must fall back, got %+v", r)
	}
	if r.notice != nil {
		t.Fatalf("an unconfigured backend produced notice %+v; want none", r.notice)
	}
}

// TestDiscoverCopilotModels_SuccessClearsNotice proves a recovered seat is not
// left labelled by a stale cache entry.
func TestDiscoverCopilotModels_SuccessClearsNotice(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return []byte(`{"models":[{"id":"gpt-5.4","policyState":"enabled"}]}`), nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverCopilotModels()
	if r.fallback || r.notice != nil {
		t.Fatalf("a successful probe must be live and unlabelled, got %+v", r)
	}
}

// TestFetchCopilotModels_PreservesRejectionBody proves the definitive sentence
// survives the probe. Before #6500 the body was discarded and the error read
// only "upstream returned 403", which cannot tell a dead seat from a wrong
// token — so nothing downstream could classify it.
func TestFetchCopilotModels_PreservesRejectionBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("unauthorized: not licensed to use Copilot\n"))
	}))
	defer srv.Close()

	_, err := fetchCopilotModels(srv.URL+"/models", "tok", "vscode-chat")
	if err == nil {
		t.Fatal("expected an error from a 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %q, want the status preserved", err)
	}
	if !strings.Contains(err.Error(), "not licensed to use Copilot") {
		t.Errorf("err = %q, want the upstream explanation preserved", err)
	}
	if copilotProbeNotice(err) == nil {
		t.Errorf("err = %q is not classifiable; the probe and classifier have drifted", err)
	}
}

// TestErrorBodySnippet covers the bounding and the degrade-to-status-only case.
func TestErrorBodySnippet(t *testing.T) {
	if got := errorBodySnippet(strings.NewReader("")); got != "" {
		t.Errorf("empty body = %q, want empty so the message has no dangling parens", got)
	}
	if got := errorBodySnippet(strings.NewReader("   \n  ")); got != "" {
		t.Errorf("whitespace body = %q, want empty", got)
	}
	if got := errorBodySnippet(strings.NewReader("a\nb\nc")); got != "(a b c)" {
		t.Errorf("multiline body = %q, want it flattened to one line", got)
	}
	long := strings.Repeat("x", copilotErrorBodyLimit*3)
	got := errorBodySnippet(strings.NewReader(long))
	if len(got) > copilotErrorBodyLimit+2 { // +2 for the wrapping parens
		t.Errorf("snippet len = %d, want it bounded by %d", len(got), copilotErrorBodyLimit)
	}
}

// TestHandleBackends_SurfacesCopilotNotice pins the wire contract the dropdown
// reads. Without this the fix stops at the server log, which is exactly the
// surface nobody was reading when the fleet went quiet.
func TestHandleBackends_SurfacesCopilotNotice(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return nil, errors.New("upstream returned 403 (unauthorized: not licensed to use Copilot)")
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}

	rec := httptest.NewRecorder()
	s.handleBackends(rec, httptest.NewRequest(http.MethodGet, "/api/config/backends", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var backends []struct {
		ID       string   `json:"id"`
		Models   []string `json:"models"`
		Fallback bool     `json:"fallback"`
		Notice   *struct {
			Class  string `json:"class"`
			Label  string `json:"label"`
			Detail string `json:"detail"`
		} `json:"notice"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &backends); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}

	var sawCopilot bool
	for _, b := range backends {
		if b.ID == "copilot" {
			sawCopilot = true
			if !b.Fallback {
				t.Error("copilot: fallback = false after a rejected probe")
			}
			if len(b.Models) == 0 {
				t.Error("copilot: empty model list; the dropdown must never blank")
			}
			if b.Notice == nil {
				t.Fatal("copilot: no notice on the wire — the dropdown cannot explain itself")
			}
			if b.Notice.Class != "auth" || b.Notice.Label == "" || b.Notice.Detail == "" {
				t.Errorf("copilot: notice = %+v, want a populated auth notice", b.Notice)
			}
			continue
		}
		// Every other backend answers from its own probe. None of them was
		// rejected here, so none may carry a notice — a notice that leaked
		// across backends would mislabel a healthy dropdown.
		if b.Notice != nil {
			t.Errorf("%s: unexpected notice %+v", b.ID, b.Notice)
		}
	}
	if !sawCopilot {
		t.Fatal("copilot missing from /api/config/backends")
	}
}

// TestBackendNoticeWiredInDashboard pins the client half. The server can carry
// the rejection perfectly and the owner still learns nothing if the dropdown
// keeps printing "(common alias, unverified)" over a dead seat — which is the
// failure #6500 actually reported.
func TestBackendNoticeWiredInDashboard(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	for _, want := range []string{
		"BACKEND_MODEL_NOTICES",           // the per-backend store
		"function setBackendModelNotice(", // records it + refreshes live dropdowns
		"function applyModelNoticeTitle(", // puts the detail on the select tooltip
		"b.notice && b.notice.label",      // the label replaces the generic suffix
		"setBackendModelNotice(b.id",      // actually invoked from the backends fetch
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing backend-notice marker %q", want)
		}
	}

	// The generic "unverified" suffix must remain for an ordinary fallback —
	// the notice narrows a case, it does not replace the existing signal.
	if !strings.Contains(body, "(common alias, unverified)") {
		t.Error("the ordinary fallback label was removed; a probe miss still needs to say it is unverified")
	}
}
