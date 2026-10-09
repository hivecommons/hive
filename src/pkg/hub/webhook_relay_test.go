package hub

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type relayedWebhook struct {
	path, event, signature string
	body                   []byte
}

func TestHandleGitHubWebhookRelaysPREventsToManagingSpoke(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(webhookSecretEnvVar, secret)

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
