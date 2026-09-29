package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const npsTestGA4ID = "G-TEST12345"

// npsFakeFiler records every CreateIssue call.
type npsFakeFiler struct {
	mu    sync.Mutex
	err   error
	calls []npsFiledIssue
}

type npsFiledIssue struct {
	repo, title, body string
	labels            []string
}

func (f *npsFakeFiler) CreateIssue(_ context.Context, repo, title, body string, labels []string) (ghpkg.CreateIssueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, npsFiledIssue{repo: repo, title: title, body: body, labels: labels})
	if f.err != nil {
		return ghpkg.CreateIssueResult{}, f.err
	}
	return ghpkg.CreateIssueResult{Number: 42, URL: "https://github.com/acme/widgets/issues/42"}, nil
}

func (f *npsFakeFiler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// npsIssueServer is a hosted (NPS on) spoke with a fake filer and, when
// enabled, the detractor-issue opt-in pointed at acme/widgets.
func npsIssueServer(t *testing.T, enabled bool) (*Server, *npsFakeFiler) {
	t.Helper()
	t.Setenv(config.NPSGA4MeasurementIDEnvVar, "")
	s := npsServer(t, newNPSFakeHub(t), config.HiveTypeHosted)
	if enabled {
		s.deps.Config.Hub.NPSDetractorIssues = config.NPSDetractorIssuesConfig{Enabled: true, Repo: "acme/widgets"}
	}
	f := &npsFakeFiler{}
	s.npsIssueFiler = f
	return s, f
}

func npsIssuePost(s *Server, body, user, role string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/feedback/nps/issue", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
		req.Header.Set("X-Hive-Role", role)
	}
	rec := httptest.NewRecorder()
	s.handleNPSIssue(rec, req)
	return rec
}

// npsLongFeedback is comfortably over npsIssueMinFeedbackRunes.
const npsLongFeedback = "The queue view never loads for our org and the agents look idle."

// TestNPSStatusTimingDefaultsAndOverrides: the status response carries the
// effective timing, equal to the dashboard constants by default and to the
// operator's hub.nps_timing when set.
func TestNPSStatusTimingDefaultsAndOverrides(t *testing.T) {
	s, _ := npsIssueServer(t, false)
	got := npsStatus(t, s, "alice", "owner")
	want := npsTimingPayload{
		MinSessions:                  2,
		SecondSessionMinEngagementMS: 5 * 60 * 1000,
		ReturningMinEngagementMS:     60 * 1000,
		RepromptDays:                 30,
		DismissRetryDays:             7,
		MaxDismissals:                3,
	}
	if got.Timing == nil || *got.Timing != want {
		t.Fatalf("default timing = %+v, want %+v", got.Timing, want)
	}

	s.deps.Config.Hub.NPSTiming = config.NPSTimingConfig{MinSessions: 1, ReturningEngagementSeconds: 30, RepromptDays: 90}
	got = npsStatus(t, s, "alice", "owner")
	want.MinSessions = 1
	want.ReturningMinEngagementMS = 30 * 1000
	want.RepromptDays = 90
	if got.Timing == nil || *got.Timing != want {
		t.Fatalf("overridden timing = %+v, want %+v", got.Timing, want)
	}
}

// TestNPSStatusExtrasOnlyForSubmitters: an anonymous or read-only viewer
// learns nothing about timing, analytics or issue settings.
func TestNPSStatusExtrasOnlyForSubmitters(t *testing.T) {
	s, _ := npsIssueServer(t, true)
	t.Setenv(config.NPSGA4MeasurementIDEnvVar, npsTestGA4ID)
	for _, tc := range []struct{ user, role string }{{"", ""}, {"viewer", "read"}} {
		got := npsStatus(t, s, tc.user, tc.role)
		if got.Timing != nil || got.GA4MeasurementID != "" || got.DetractorIssue != nil {
			t.Errorf("user=%q role=%q: extras leaked: %+v", tc.user, tc.role, got)
		}
	}
	if got := npsStatus(t, s, "alice", "owner"); got.Timing == nil || got.GA4MeasurementID != npsTestGA4ID || got.DetractorIssue == nil {
		t.Errorf("positive control: owner should get every extra, got %+v", got)
	}
}

// TestNPSStatusGA4OffByDefault: no measurement ID means the dashboard is
// never told to load anything; a malformed one is treated as unset.
func TestNPSStatusGA4OffByDefault(t *testing.T) {
	s, _ := npsIssueServer(t, false)
	if got := npsStatus(t, s, "alice", "owner"); got.GA4MeasurementID != "" {
		t.Errorf("unset: ga4_measurement_id = %q, want empty", got.GA4MeasurementID)
	}
	t.Setenv(config.NPSGA4MeasurementIDEnvVar, "not-an-id")
	if got := npsStatus(t, s, "alice", "owner"); got.GA4MeasurementID != "" {
		t.Errorf("malformed: ga4_measurement_id = %q, want empty", got.GA4MeasurementID)
	}
	t.Setenv(config.NPSGA4MeasurementIDEnvVar, npsTestGA4ID)
	if got := npsStatus(t, s, "alice", "owner"); got.GA4MeasurementID != npsTestGA4ID {
		t.Errorf("set: ga4_measurement_id = %q, want %q", got.GA4MeasurementID, npsTestGA4ID)
	}
}

// TestNPSCSPWidenedOnlyWhenGA4Configured: with no measurement ID the policy
// names no Google host at all; with one, only script-src(-elem) and
// connect-src gain Google's tag hosts, and /terminal is untouched.
func TestNPSCSPWidenedOnlyWhenGA4Configured(t *testing.T) {
	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	csp := func(path string) string {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Header().Get("Content-Security-Policy")
	}

	t.Setenv(config.NPSGA4MeasurementIDEnvVar, "")
	off := csp("/")
	if strings.Contains(off, "google") {
		t.Fatalf("GA4 unset but the CSP names a Google host:\n%s", off)
	}
	if got := cspDirective(off, "connect-src"); got != "connect-src 'self' ws: wss:" {
		t.Errorf("GA4 unset: connect-src = %q, want it unchanged", got)
	}

	t.Setenv(config.NPSGA4MeasurementIDEnvVar, npsTestGA4ID)
	on := csp("/")
	for _, d := range []string{"script-src", "script-src-elem"} {
		if got := cspDirective(on, d); !strings.Contains(got, npsGA4ScriptSources) {
			t.Errorf("GA4 set: %s = %q, want it to allow %s", d, got, npsGA4ScriptSources)
		}
	}
	if got := cspDirective(on, "connect-src"); !strings.Contains(got, "https://*.google-analytics.com") {
		t.Errorf("GA4 set: connect-src = %q, want the GA4 collection host", got)
	}
	if got := cspDirective(on, "script-src"); strings.Contains(got, "'sha256-") || strings.Contains(got, "'unsafe-inline'") {
		t.Errorf("GA4 set: script-src fallback must stay hash-free and without unsafe-inline, got %q", got)
	}
	for _, d := range []string{"default-src", "img-src", "frame-ancestors", "script-src-attr"} {
		if cspDirective(on, d) != cspDirective(off, d) {
			t.Errorf("GA4 set changed %s: %q -> %q", d, cspDirective(off, d), cspDirective(on, d))
		}
	}
	if term := csp("/terminal"); strings.Contains(term, "google") {
		t.Errorf("/terminal CSP must never gain Google hosts:\n%s", term)
	}
}

// TestNPSIssueOffByDefault: without hub.nps_detractor_issues the status says
// nothing and the endpoint refuses, without touching the forge.
func TestNPSIssueOffByDefault(t *testing.T) {
	s, f := npsIssueServer(t, false)
	if got := npsStatus(t, s, "alice", "owner"); got.DetractorIssue != nil {
		t.Errorf("status offers detractor issues while off: %+v", got.DetractorIssue)
	}
	rec := npsIssuePost(s, `{"score":1,"feedback":"`+npsLongFeedback+`","consent":true}`, "alice", "owner")
	if rec.Code != http.StatusForbidden {
		t.Errorf("off: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if f.count() != 0 {
		t.Fatalf("off: forge called %d times", f.count())
	}
}

// TestNPSIssueRequiresNPSAndForge: the opt-in alone is not enough; NPS must
// be enabled for the hive and a forge client present.
func TestNPSIssueRequiresNPSAndForge(t *testing.T) {
	s, f := npsIssueServer(t, true)
	s.deps.Config.Hub.HiveType = "" // standalone: NPS defaults off
	if rec := npsIssuePost(s, `{"score":1,"feedback":"`+npsLongFeedback+`","consent":true}`, "alice", "owner"); rec.Code != http.StatusForbidden {
		t.Errorf("NPS off: status = %d, want 403", rec.Code)
	}
	if f.count() != 0 {
		t.Fatalf("NPS off: forge called %d times", f.count())
	}

	s, _ = npsIssueServer(t, true)
	s.npsIssueFiler = nil // and deps.GHClient is nil
	if got := npsStatus(t, s, "alice", "owner"); got.DetractorIssue != nil {
		t.Errorf("no forge client: status offers detractor issues: %+v", got.DetractorIssue)
	}
	if rec := npsIssuePost(s, `{"score":1,"feedback":"`+npsLongFeedback+`","consent":true}`, "alice", "owner"); rec.Code != http.StatusForbidden {
		t.Errorf("no forge client: status = %d, want 403", rec.Code)
	}
}

// TestNPSIssueValidation: only a signed-in writer, only score 1, only with
// consent, only with enough text. None of the refusals reaches the forge.
func TestNPSIssueValidation(t *testing.T) {
	s, f := npsIssueServer(t, true)
	if got := npsStatus(t, s, "alice", "owner"); got.DetractorIssue == nil ||
		got.DetractorIssue.Score != npsDetractorScore || got.DetractorIssue.MinFeedbackChars != npsIssueMinFeedbackRunes ||
		got.DetractorIssue.Repo != "acme/widgets" {
		t.Fatalf("positive control: status should offer detractor issues, got %+v", got.DetractorIssue)
	}
	exactlyMin := strings.Repeat("x", npsIssueMinFeedbackRunes)
	cases := []struct {
		name, body, user, role string
		want                   int
	}{
		{"anonymous", `{"score":1,"feedback":"` + npsLongFeedback + `","consent":true}`, "", "", http.StatusForbidden},
		{"read-only", `{"score":1,"feedback":"` + npsLongFeedback + `","consent":true}`, "viewer", "read", http.StatusForbidden},
		{"no consent", `{"score":1,"feedback":"` + npsLongFeedback + `"}`, "alice", "owner", http.StatusBadRequest},
		{"consent false", `{"score":1,"feedback":"` + npsLongFeedback + `","consent":false}`, "alice", "owner", http.StatusBadRequest},
		{"consent as string", `{"score":1,"feedback":"` + npsLongFeedback + `","consent":"yes"}`, "alice", "owner", http.StatusBadRequest},
		{"passive score", `{"score":2,"feedback":"` + npsLongFeedback + `","consent":true}`, "alice", "owner", http.StatusBadRequest},
		{"promoter score", `{"score":4,"feedback":"` + npsLongFeedback + `","consent":true}`, "alice", "owner", http.StatusBadRequest},
		{"missing score", `{"feedback":"` + npsLongFeedback + `","consent":true}`, "alice", "owner", http.StatusBadRequest},
		{"too short", `{"score":1,"feedback":"too short","consent":true}`, "alice", "owner", http.StatusBadRequest},
		{"short once trimmed", `{"score":1,"feedback":"   ` + exactlyMin[1:] + `   ","consent":true}`, "alice", "owner", http.StatusBadRequest},
		{"control chars do not count", `{"score":1,"feedback":"` + exactlyMin[1:] + `\u0007","consent":true}`, "alice", "owner", http.StatusBadRequest},
		{"too large", `{"score":1,"consent":true,"feedback":"` + strings.Repeat("a", npsMaxBodyBytes) + `"}`, "alice", "owner", http.StatusRequestEntityTooLarge},
		{"invalid JSON", `{`, "alice", "owner", http.StatusBadRequest},
	}
	for _, tc := range cases {
		if rec := npsIssuePost(s, tc.body, tc.user, tc.role); rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	if f.count() != 0 {
		t.Fatalf("refused requests reached the forge %d times", f.count())
	}

	// Exactly the minimum is accepted (boundary positive control).
	if rec := npsIssuePost(s, `{"score":1,"feedback":"`+exactlyMin+`","consent":true}`, "alice", "owner"); rec.Code != http.StatusOK {
		t.Fatalf("exactly %d chars: status = %d, want 200 (%s)", npsIssueMinFeedbackRunes, rec.Code, rec.Body.String())
	}
	if f.count() != 1 {
		t.Fatalf("accepted request: forge called %d times, want 1", f.count())
	}
}

// npsRawMentionRe matches an @mention GitHub would notify: "@" not preceded
// by a word character or backtick (emails like a@b.c are not mentions).
var npsRawMentionRe = regexp.MustCompile("(^|[^A-Za-z0-9_`])@[A-Za-z0-9]")

// TestNPSIssueFiledSanitized: the issue goes to the configured repo through
// the forge, with every @mention neutralized in title and body, and without
// the user's identity.
func TestNPSIssueFiledSanitized(t *testing.T) {
	s, f := npsIssueServer(t, true)
	feedback := `@octocat and @hivecommons/maintainers: the queue view is broken\nsee also @someone-else ` + "`@inline`"
	rec := npsIssuePost(s, `{"score":1,"feedback":"`+feedback+`","consent":true}`, "alice-the-user", "owner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "https://github.com/acme/widgets/issues/42") {
		t.Errorf("response does not carry the issue URL: %s", rec.Body.String())
	}
	if f.count() != 1 {
		t.Fatalf("forge called %d times, want 1", f.count())
	}
	got := f.calls[0]
	if got.repo != "acme/widgets" {
		t.Errorf("repo = %q, want acme/widgets", got.repo)
	}
	if len(got.labels) != 0 {
		t.Errorf("labels = %v, want none (no labels created in someone's repo)", got.labels)
	}
	for name, text := range map[string]string{"title": got.title, "body": got.body} {
		if m := npsRawMentionRe.FindString(text); m != "" {
			t.Errorf("%s carries a raw mention %q:\n%s", name, m, text)
		}
		if strings.Contains(text, "alice-the-user") {
			t.Errorf("%s carries the submitting user's identity:\n%s", name, text)
		}
		if strings.Contains(text, "hive-one") {
			t.Errorf("%s carries the hive ID:\n%s", name, text)
		}
	}
	if !strings.HasPrefix(got.title, npsIssueTitlePrefix) || strings.Contains(got.title, "\n") {
		t.Errorf("title %q must start with %q and be one line", got.title, npsIssueTitlePrefix)
	}
	if !strings.Contains(got.body, "`octocat`") || !strings.Contains(got.body, "queue view is broken") {
		t.Errorf("body lost the (neutralized) feedback:\n%s", got.body)
	}
	if !strings.Contains(got.body, "explicit consent") {
		t.Errorf("body does not say it was filed with consent:\n%s", got.body)
	}
}

// TestNPSIssueRateLimited: one public issue per user per window and a small
// per-hive cap; a forge failure does not burn the user's slot.
func TestNPSIssueRateLimited(t *testing.T) {
	s, f := npsIssueServer(t, true)
	body := `{"score":1,"feedback":"` + npsLongFeedback + `","consent":true}`

	f.err = errors.New("forge down")
	if rec := npsIssuePost(s, body, "alice", "owner"); rec.Code != http.StatusBadGateway {
		t.Fatalf("forge failure: status = %d, want 502", rec.Code)
	}
	f.err = nil
	if rec := npsIssuePost(s, body, "alice", "owner"); rec.Code != http.StatusOK {
		t.Fatalf("retry after forge failure: status = %d, want 200 (slot must be released)", rec.Code)
	}
	if rec := npsIssuePost(s, body, "ALICE", "owner"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second issue by the same user: status = %d, want 429", rec.Code)
	}
	for i := 1; i < npsIssueMaxPerHivePerWindow; i++ {
		if rec := npsIssuePost(s, body, "user-"+string(rune('a'+i)), "owner"); rec.Code != http.StatusOK {
			t.Fatalf("user %d: status = %d, want 200", i, rec.Code)
		}
	}
	if rec := npsIssuePost(s, body, "one-too-many", "owner"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the hive cap: status = %d, want 429", rec.Code)
	}
	if want := 1 + npsIssueMaxPerHivePerWindow; f.count() != want {
		t.Fatalf("forge called %d times, want %d", f.count(), want)
	}
}

// TestNPSIssueRouteRegistered pins the route on the real mux.
func TestNPSIssueRouteRegistered(t *testing.T) {
	b, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `s.mux.HandleFunc("POST /api/feedback/nps/issue", s.handleNPSIssue)`) {
		t.Error("POST /api/feedback/nps/issue is not registered in api.go")
	}
}
