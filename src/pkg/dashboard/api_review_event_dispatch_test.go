package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review/eventdispatch"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

func reviewEventTestServer(t *testing.T, eventDriven *bool) (*Server, *eventdispatch.Dispatcher) {
	t.Helper()
	s := newGitHubWebhookTestServer(t)
	cfg := &config.Config{}
	cfg.Review.RequireApproval = true
	cfg.Review.FanOut = true
	cfg.Review.EventDriven = eventDriven
	cfg.Review.EventDebounceS = 45
	d := eventdispatch.New(eventdispatch.Options{})
	s.deps.Config = cfg
	s.deps.ReviewEvents = d
	return s, d
}

func prWebhookBody(action, sha, state string, draft bool) []byte {
	return fmt.Appendf(nil, `{"action":%q,"number":9,"repository":{"full_name":"webhookorg/webhookrepo"},"pull_request":{"number":9,"state":%q,"draft":%t,"head":{"sha":%q}}}`,
		action, state, draft, sha)
}

func TestGitHubWebhookQueuesReviewDispatch(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	off := false
	tests := []struct {
		name        string
		eventDriven *bool
		event       string
		bodies      [][]byte
		want        []string
	}{
		{
			name:   "push burst coalesces",
			event:  "pull_request",
			bodies: [][]byte{prWebhookBody("opened", "a1", "open", false), prWebhookBody("synchronize", "a2", "open", false), prWebhookBody("review_requested", "a2", "open", false)},
			want:   []string{eventdispatch.OutcomeQueued, eventdispatch.OutcomeCoalesced, eventdispatch.OutcomeCoalesced},
		},
		{
			name:   "draft and closed PRs are not queued",
			event:  "pull_request",
			bodies: [][]byte{prWebhookBody("synchronize", "a1", "open", true), prWebhookBody("synchronize", "a1", "closed", false)},
			want:   []string{"", ""},
		},
		{
			name:   "non-review actions are not queued",
			event:  "pull_request",
			bodies: [][]byte{prWebhookBody("labeled", "a1", "open", false), []byte(`{"action":"opened","repository":{"full_name":"webhookorg/webhookrepo"},"number":9}`)},
			want:   []string{"", ""},
		},
		{
			name:   "other events are not queued",
			event:  "pull_request_review",
			bodies: [][]byte{prWebhookBody("submitted", "a1", "open", false)},
			want:   []string{""},
		},
		{
			name:        "event_driven off",
			eventDriven: &off,
			event:       "pull_request",
			bodies:      [][]byte{prWebhookBody("synchronize", "a1", "open", false)},
			want:        []string{""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, d := reviewEventTestServer(t, tc.eventDriven)
			queued := 0
			for i, body := range tc.bodies {
				rec := postGitHubWebhook(s, tc.event, signGitHubWebhookForTest(secret, body), body)
				if rec.Code != http.StatusOK {
					t.Fatalf("delivery %d: status = %d (%s)", i, rec.Code, rec.Body.String())
				}
				var resp map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				got, _ := resp["review_dispatch"].(string)
				if got != tc.want[i] {
					t.Fatalf("delivery %d: review_dispatch = %q, want %q", i, got, tc.want[i])
				}
				if got == eventdispatch.OutcomeQueued {
					queued++
				}
			}
			st := d.Snapshot()
			if len(st.Pending) != queued {
				t.Fatalf("pending = %+v, want %d", st.Pending, queued)
			}
			if queued == 1 {
				p := st.Pending[0]
				if p.Repo != "webhookorg/webhookrepo" || p.Number != 9 || p.HeadSHA != "a2" || p.Coalesced != 2 {
					t.Fatalf("pending entry = %+v", p)
				}
				if got := p.DueAt.Sub(p.LastSeen); got != 45*time.Second {
					t.Fatalf("debounce = %v, want review.event_debounce_s", got)
				}
			}
		})
	}
}

func TestReviewEventDispatchEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		secret      string
		withDeps    bool
		wantEnabled bool
		wantHooks   bool
		wantDebounc int
		wantPending int
	}{
		{name: "webhooks configured", secret: "x", withDeps: true, wantEnabled: true, wantHooks: true, wantDebounc: 45, wantPending: 1},
		{name: "no webhook secret", secret: "", withDeps: true, wantEnabled: false, wantHooks: false, wantDebounc: 45, wantPending: 1},
		{name: "no dispatcher", secret: "x", withDeps: false, wantHooks: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(githubWebhookSecretEnvVar, tc.secret)
			s, d := reviewEventTestServer(t, nil)
			d.Enqueue(eventdispatch.Request{Repo: "o/r", Number: 1, HeadSHA: "a"}, time.Minute)
			if !tc.withDeps {
				s.deps.ReviewEvents = nil
			}
			rec := httptest.NewRecorder()
			s.handleReviewEventDispatch(rec, httptest.NewRequest(http.MethodGet, reviewEventDispatchPath, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			var got struct {
				Enabled            bool                  `json:"enabled"`
				WebhooksConfigured bool                  `json:"webhooks_configured"`
				DebounceS          int                   `json:"debounce_s"`
				Pending            []eventdispatch.Entry `json:"pending"`
				Recent             []eventdispatch.Entry `json:"recent"`
				Stats              eventdispatch.Stats   `json:"stats"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Enabled != tc.wantEnabled || got.WebhooksConfigured != tc.wantHooks || got.DebounceS != tc.wantDebounc ||
				len(got.Pending) != tc.wantPending || got.Recent == nil {
				t.Fatalf("response = %s", rec.Body.String())
			}
		})
	}
}

func TestReviewEventDispatchRouteRegistered(t *testing.T) {
	s := newGitHubWebhookTestServer(t)
	s.registerGitHubWebhookRoutes()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, reviewEventDispatchPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", reviewEventDispatchPath, rec.Code)
	}
	if isPublicPath(reviewEventDispatchPath) {
		t.Fatal("the dispatch queue must stay behind dashboard auth")
	}
}

func TestReviewCardTrigger(t *testing.T) {
	s, _ := reviewEventTestServer(t, nil)
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	d := eventdispatch.New(eventdispatch.Options{Now: func() time.Time { return clock }})
	s.deps.ReviewEvents = d
	d.Enqueue(eventdispatch.Request{Repo: "webhookorg/fired", Number: 1, HeadSHA: "h1"}, time.Minute)
	clock = clock.Add(time.Minute)
	d.TakeDue()
	d.Enqueue(eventdispatch.Request{Repo: "webhookorg/queued", Number: 2, HeadSHA: "h2"}, time.Minute)

	reviewed := []pipeline.Reviewer{{Perspective: "correctness"}}
	tests := []struct {
		name string
		card pipeline.Card
		want string
	}{
		{"event fired for head", pipeline.Card{Repo: "fired", Number: 1, HeadSHA: "h1", Reviewers: reviewed}, eventdispatch.TriggerEvent},
		{"event queued", pipeline.Card{Repo: "queued", Number: 2, HeadSHA: "h2"}, eventdispatch.TriggerEventPending},
		{"new head after event reviewed by cadence", pipeline.Card{Repo: "fired", Number: 1, HeadSHA: "h9", Reviewers: reviewed}, eventdispatch.TriggerCadence},
		{"unreviewed, no event", pipeline.Card{Repo: "other", Number: 3, HeadSHA: "h3"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.reviewCardTrigger(tc.card); got != tc.want {
				t.Fatalf("trigger = %q, want %q", got, tc.want)
			}
		})
	}

	var nilSrv *Server
	if nilSrv.reviewEvents() != nil {
		t.Fatal("nil server has no dispatcher")
	}
	if on, _ := nilSrv.reviewEventSettings(); on {
		t.Fatal("nil server cannot enable event dispatch")
	}
	if got := nilSrv.reviewCardTrigger(pipeline.Card{Number: 1}); got != "" {
		t.Fatalf("nil server trigger = %q", got)
	}
}
