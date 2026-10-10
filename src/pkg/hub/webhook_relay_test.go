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
func TestRelayWebhookToSpokeRefusesPrivateDashboardURL(t *testing.T) {
	hit := make(chan struct{}, 1)
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer spoke.Close()

	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1", Org: "acme", Repos: []string{"widgets"}, DashboardURL: spoke.URL}}
	for _, u := range []string{spoke.URL, "http://169.254.169.254", "http://10.0.0.5:8080", "http://localhost:8080"} {
		s.registry.Hives[0].DashboardURL = u
		s.relayWebhookToSpoke("pull_request", http.Header{}, []byte(`{"repository":{"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}}`))
	}
	select {
	case <-hit:
		t.Fatal("relay reached a private dashboard URL; the SSRF guard must refuse it")
	case <-time.After(500 * time.Millisecond):
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
