package hub

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// allowLoopbackRelay disables the private-address relay guard for the
// duration of a test so an httptest spoke on 127.0.0.1 can receive the relay.
func allowLoopbackRelay(t *testing.T) {
	t.Helper()
	orig := spokeWebhookRelayGuard
	spokeWebhookRelayGuard = func(context.Context, string) bool { return false }
	t.Cleanup(func() { spokeWebhookRelayGuard = orig })
}

type relayedWebhook struct {
	path, event, signature string
	body                   []byte
}

func TestHandleGitHubWebhookRelaysPREventsToManagingSpoke(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(webhookSecretEnvVar, secret)
	allowLoopbackRelay(t)

	got := make(chan relayedWebhook, 4)
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- relayedWebhook{path: r.URL.Path, event: r.Header.Get("X-GitHub-Event"), signature: r.Header.Get("X-Hub-Signature-256"), body: body}
		w.WriteHeader(http.StatusOK)
	}))
	defer spoke.Close()

	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1", Org: "acme", Repos: []string{"widgets"}, DashboardURL: spoke.URL + "/"}}

	body := []byte(`{"action":"synchronize","number":7,"repository":{"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}}`)
	sig := signWebhook(secret, body)
	req := httptest.NewRequest(http.MethodPost, "/api/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	select {
	case r := <-got:
		if r.path != spokeWebhookPath || r.event != "pull_request" || r.signature != sig || !bytes.Equal(r.body, body) {
			t.Fatalf("relayed %+v, want the original signed delivery on %s", r, spokeWebhookPath)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pull_request delivery was not relayed to the managing spoke")
	}
}

func TestRelayWebhookToSpokeIgnoresUnmanagedRepos(t *testing.T) {
	allowLoopbackRelay(t)
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected relay to spoke for %s", r.Header.Get("X-GitHub-Event"))
	}))
	defer spoke.Close()

	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Org: "acme", Repos: []string{"widgets"}, DashboardURL: spoke.URL},
		{ID: "h2", Org: "acme", Repos: []string{"gadgets"}},
	}
	for _, body := range []string{
		`{}`,
		`not json`,
		`{"repository":{"name":"other","full_name":"acme/other","owner":{"login":"acme"}}}`,
		`{"repository":{"name":"widgets","full_name":"elsewhere/widgets","owner":{"login":"elsewhere"}}}`,
		`{"repository":{"name":"gadgets","full_name":"acme/gadgets"}}`,
		`{"repository":{"full_name":"acme/nameless"}}`,
	} {
		s.relayWebhookToSpoke("push", http.Header{}, []byte(body))
	}
}

// TestRelayWebhookToSpokeRefusesPrivateDashboardURL: DashboardURL is
// self-reported by the spoke, so a private/internal target must never be
// relayed to. The production guard (isPrivateURL) rejects loopback, so the
// httptest spoke here must receive nothing.
//
// The relay evaluates the guard on its own goroutine, so this test wraps the
// production guard (still isPrivateURL underneath) to observe each verdict and
// waits for every relay to finish evaluating before returning. Without that
// wait the goroutines outlived the test and raced the next test's
// allowLoopbackRelay swap of spokeWebhookRelayGuard under -race.
func TestRelayWebhookToSpokeRefusesPrivateDashboardURL(t *testing.T) {
	hit := make(chan struct{}, 1)
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer spoke.Close()

	targets := []string{spoke.URL, "http://169.254.169.254", "http://10.0.0.5:8080", "http://localhost:8080"}
	verdicts := make(chan bool, len(targets))
	orig := spokeWebhookRelayGuard
	spokeWebhookRelayGuard = func(ctx context.Context, u string) bool {
		refused := orig(ctx, u)
		verdicts <- refused
		return refused
	}
	t.Cleanup(func() { spokeWebhookRelayGuard = orig })

	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1", Org: "acme", Repos: []string{"widgets"}, DashboardURL: spoke.URL}}
	for _, u := range targets {
		s.registry.Hives[0].DashboardURL = u
		s.relayWebhookToSpoke("pull_request", http.Header{}, []byte(`{"repository":{"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}}`))
	}
	deadline := time.After(5 * time.Second)
	for i := range targets {
		select {
		case refused := <-verdicts:
			if !refused {
				t.Errorf("guard allowed a private dashboard URL (verdict %d of %d)", i+1, len(targets))
			}
		case <-hit:
			t.Fatal("relay reached a private dashboard URL; the SSRF guard must refuse it")
		case <-deadline:
			t.Fatalf("only %d of %d relays evaluated the guard before the deadline", i, len(targets))
		}
	}
	select {
	case <-hit:
		t.Fatal("relay reached a private dashboard URL; the SSRF guard must refuse it")
	default:
	}
}

// TestRelayWebhookToSpokeDoesNotFollowRedirects: a 30x from the spoke URL must
// not be followed, otherwise a public DashboardURL could bounce the hub to an
// internal host after the guard has already passed.
func TestRelayWebhookToSpokeDoesNotFollowRedirects(t *testing.T) {
	allowLoopbackRelay(t)
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect target received relayed %s %s", r.Method, r.URL.Path)
	}))
	defer internal.Close()
	redirected := make(chan struct{}, 1)
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected <- struct{}{}
		http.Redirect(w, r, internal.URL+"/admin", http.StatusTemporaryRedirect)
	}))
	defer spoke.Close()

	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1", Org: "acme", Repos: []string{"widgets"}, DashboardURL: spoke.URL}}
	s.relayWebhookToSpoke("pull_request", http.Header{}, []byte(`{"repository":{"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}}`))
	select {
	case <-redirected:
	case <-time.After(5 * time.Second):
		t.Fatal("spoke never received the relay")
	}
	// Give a (wrongly) followed redirect time to land before the test ends.
	<-time.After(200 * time.Millisecond)
}

func TestRelayWebhookToSpokeFansOutToEveryTrackingHive(t *testing.T) {
	allowLoopbackRelay(t)
	got := make(chan relayedWebhook, 4)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- relayedWebhook{path: r.URL.Path, event: r.Header.Get("X-GitHub-Event"), signature: r.Header.Get("X-Hub-Signature-256"), body: body}
		w.WriteHeader(http.StatusOK)
	})
	spokeA := httptest.NewServer(handler)
	defer spokeA.Close()
	spokeB := httptest.NewServer(handler)
	defer spokeB.Close()

	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Org: "acme", Repos: []string{"widgets"}, DashboardURL: spokeA.URL},
		{ID: "nodash", Org: "acme", Repos: []string{"widgets"}},
		{ID: "h2", Org: "acme", PrimaryRepo: "widgets", DashboardURL: spokeB.URL},
		{ID: "other", Org: "acme", Repos: []string{"gadgets"}, DashboardURL: spokeB.URL},
	}
	body := []byte(`{"repository":{"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}}`)
	hdr := http.Header{}
	hdr.Set("X-GitHub-Event", "pull_request")
	hdr.Set("X-Hub-Signature-256", "sha256=abc")
	s.relayWebhookToSpoke("pull_request", hdr, body)

	for i := 0; i < 2; i++ {
		select {
		case r := <-got:
			if r.path != spokeWebhookPath || r.event != "pull_request" || r.signature != "sha256=abc" || !bytes.Equal(r.body, body) {
				t.Fatalf("relayed %+v, want the original delivery on %s", r, spokeWebhookPath)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 tracking hives received the relay", i)
		}
	}
	select {
	case r := <-got:
		t.Fatalf("unexpected third relay: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFindHivesByOrgRepos(t *testing.T) {
	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Org: "acme", Repos: []string{"widgets"}},
		{ID: "h2", Org: "acme", PrimaryRepo: "Widgets"},
		{ID: "h3", Org: "acme", Repos: []string{"gadgets"}},
	}
	ids := func(hs []*RegistryEntry) string {
		out := ""
		for _, h := range hs {
			out += h.ID + ","
		}
		return out
	}
	if got := ids(s.findHivesByOrgRepos("acme", []string{"widgets"})); got != "h1,h2," {
		t.Errorf("exact match = %q, want h1,h2,", got)
	}
	if got := ids(s.findHivesByOrgRepos("acme", nil)); got != "h1,h2,h3," {
		t.Errorf("org-only fallback = %q, want h1,h2,h3,", got)
	}
	if got := s.findHivesByOrgRepos("acme", []string{"unknown"}); len(got) != 0 {
		t.Errorf("unknown repo matched %q", ids(got))
	}
	if h := s.findHiveByOrgRepos("acme", nil); h == nil || h.ID != "h1" {
		t.Errorf("single lookup org fallback = %+v, want h1", h)
	}
}
