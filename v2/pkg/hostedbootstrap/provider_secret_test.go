package hostedbootstrap

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v72/github"
	"golang.org/x/crypto/nacl/box"
)

func TestEnsureProviderSecretCreatesOnce(t *testing.T) {
	harness := newProviderSecretHarness(t)
	server, client := harness.serverAndClient(t)
	defer server.Close()
	ctx, cancel := providerTestContext(t)
	defer cancel()

	plaintext := []byte("sk" + "-test-value-that-must-not-escape")
	result, err := EnsureProviderSecret(ctx, client, "acme", "widget", plaintext)
	if err != nil {
		t.Fatalf("EnsureProviderSecret() error = %v", err)
	}
	if result.SecretName != ProviderSecretName || result.AlreadyConfigured {
		t.Fatalf("unexpected result: %+v", result)
	}
	if string(plaintext) != "sk"+"-test-value-that-must-not-escape" {
		t.Fatal("EnsureProviderSecret mutated caller-owned plaintext")
	}

	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.putCount != 1 || !harness.exists {
		t.Fatalf("secret puts=%d exists=%v", harness.putCount, harness.exists)
	}
	if string(harness.opened) != string(plaintext) {
		t.Fatal("sealed request did not contain the expected provider credential")
	}
}

func TestEnsureProviderSecretExistingIsNoOpWithEmptyInput(t *testing.T) {
	harness := newProviderSecretHarness(t)
	harness.exists = true
	server, client := harness.serverAndClient(t)
	defer server.Close()
	ctx, cancel := providerTestContext(t)
	defer cancel()

	result, err := EnsureProviderSecret(ctx, client, "acme", "widget", nil)
	if err != nil {
		t.Fatalf("EnsureProviderSecret() error = %v", err)
	}
	if !result.AlreadyConfigured || result.SecretName != ProviderSecretName {
		t.Fatalf("unexpected idempotent result: %+v", result)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.publicKeyGets != 0 || harness.putCount != 0 {
		t.Fatalf("existing secret was touched: public-key gets=%d puts=%d", harness.publicKeyGets, harness.putCount)
	}
}

func TestEnsureProviderSecretRejectsMissingValue(t *testing.T) {
	harness := newProviderSecretHarness(t)
	server, client := harness.serverAndClient(t)
	defer server.Close()
	ctx, cancel := providerTestContext(t)
	defer cancel()

	_, err := EnsureProviderSecret(ctx, client, "acme", "widget", nil)
	if err == nil || !strings.Contains(err.Error(), "credential is required") {
		t.Fatalf("EnsureProviderSecret() error = %v", err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.publicKeyGets != 0 || harness.putCount != 0 {
		t.Fatalf("missing credential reached provisioning: key gets=%d puts=%d", harness.publicKeyGets, harness.putCount)
	}
}

func TestEnsureProviderSecretRejectsConcurrentAppearance(t *testing.T) {
	harness := newProviderSecretHarness(t)
	harness.existsSequence = []bool{false, true}
	server, client := harness.serverAndClient(t)
	defer server.Close()
	ctx, cancel := providerTestContext(t)
	defer cancel()

	_, err := EnsureProviderSecret(ctx, client, "acme", "widget", []byte("do-not-overwrite"))
	if err == nil || !strings.Contains(err.Error(), "appeared concurrently") {
		t.Fatalf("EnsureProviderSecret() error = %v", err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.putCount != 0 {
		t.Fatalf("concurrently appearing secret was overwritten %d times", harness.putCount)
	}
}

func TestEnsureProviderSecretReconcilesLostPutResponse(t *testing.T) {
	harness := newProviderSecretHarness(t)
	harness.putStatus = http.StatusInternalServerError
	harness.persistOnPutError = true
	server, client := harness.serverAndClient(t)
	defer server.Close()
	ctx, cancel := providerTestContext(t)
	defer cancel()

	result, err := EnsureProviderSecret(ctx, client, "acme", "widget", []byte("accepted-before-response-loss"))
	if err != nil {
		t.Fatalf("EnsureProviderSecret() error = %v", err)
	}
	if result.AlreadyConfigured || result.SecretName != ProviderSecretName {
		t.Fatalf("unexpected reconciled result: %+v", result)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.putCount != 1 || !harness.exists || harness.secretGets != 3 {
		t.Fatalf("response-loss state: puts=%d exists=%v metadata gets=%d", harness.putCount, harness.exists, harness.secretGets)
	}
}

func TestEnsureProviderSecretRejectsInvalidTargetAndKey(t *testing.T) {
	t.Run("target", func(t *testing.T) {
		harness := newProviderSecretHarness(t)
		server, client := harness.serverAndClient(t)
		defer server.Close()
		ctx, cancel := providerTestContext(t)
		defer cancel()

		_, err := EnsureProviderSecret(ctx, client, "../acme", "widget", []byte("unused"))
		if err == nil || !strings.Contains(err.Error(), "canonical names") {
			t.Fatalf("EnsureProviderSecret() error = %v", err)
		}
		harness.mu.Lock()
		defer harness.mu.Unlock()
		if len(harness.events) != 0 {
			t.Fatalf("invalid target reached GitHub: %v", harness.events)
		}
	})

	t.Run("key", func(t *testing.T) {
		harness := newProviderSecretHarness(t)
		harness.invalidKey = true
		server, client := harness.serverAndClient(t)
		defer server.Close()
		ctx, cancel := providerTestContext(t)
		defer cancel()

		plaintext := "secret-marker-must-not-leak"
		_, err := EnsureProviderSecret(ctx, client, "acme", "widget", []byte(plaintext))
		if err == nil || !strings.Contains(err.Error(), "public key is malformed") {
			t.Fatalf("EnsureProviderSecret() error = %v", err)
		}
		if strings.Contains(err.Error(), plaintext) {
			t.Fatal("provider credential leaked through error text")
		}
		harness.mu.Lock()
		defer harness.mu.Unlock()
		if harness.putCount != 0 {
			t.Fatalf("invalid key caused %d PUTs", harness.putCount)
		}
	})
}

func TestEnsureProviderSecretRejectsOversizedValue(t *testing.T) {
	harness := newProviderSecretHarness(t)
	server, client := harness.serverAndClient(t)
	defer server.Close()
	ctx, cancel := providerTestContext(t)
	defer cancel()

	_, err := EnsureProviderSecret(ctx, client, "acme", "widget", make([]byte, providerSecretMaxBytes+1))
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("EnsureProviderSecret() error = %v", err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.publicKeyGets != 0 || harness.putCount != 0 {
		t.Fatalf("oversized credential reached provisioning: key gets=%d puts=%d", harness.publicKeyGets, harness.putCount)
	}
}

func providerTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 2*time.Second)
}

type providerSecretHarness struct {
	t                 *testing.T
	mu                sync.Mutex
	publicKey         *[32]byte
	privateKey        *[32]byte
	exists            bool
	existsSequence    []bool
	secretGets        int
	publicKeyGets     int
	putCount          int
	putStatus         int
	persistOnPutError bool
	invalidKey        bool
	opened            []byte
	events            []string
}

func newProviderSecretHarness(t *testing.T) *providerSecretHarness {
	t.Helper()
	publicKey, privateKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return &providerSecretHarness{t: t, publicKey: publicKey, privateKey: privateKey}
}

func (h *providerSecretHarness) serverAndClient(t *testing.T) (*httptest.Server, *github.Client) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(h.serveHTTP))
	client := github.NewClient(nil)
	baseURL := server.URL + "/"
	var err error
	client.BaseURL, err = client.BaseURL.Parse(baseURL)
	if err != nil {
		server.Close()
		t.Fatalf("parse test API URL: %v", err)
	}
	client.UploadURL = client.BaseURL
	return server, client
}

func (h *providerSecretHarness) serveHTTP(response http.ResponseWriter, request *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, request.Method+" "+request.URL.Path)
	response.Header().Set("Content-Type", "application/json")

	const prefix = "/repos/acme/widget"
	switch {
	case request.Method == http.MethodGet && request.URL.Path == prefix+"/actions/secrets/"+ProviderSecretName:
		exists := h.exists
		if h.secretGets < len(h.existsSequence) {
			exists = h.existsSequence[h.secretGets]
		}
		h.secretGets++
		if !exists {
			h.writeJSON(response, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		h.writeJSON(response, http.StatusOK, map[string]any{"name": ProviderSecretName, "created_at": "2026-07-15T20:00:00Z", "updated_at": "2026-07-15T20:00:00Z"})
	case request.Method == http.MethodGet && request.URL.Path == prefix+"/actions/secrets/public-key":
		h.publicKeyGets++
		key := base64.StdEncoding.EncodeToString(h.publicKey[:])
		if h.invalidKey {
			key = "not-a-valid-key"
		}
		h.writeJSON(response, http.StatusOK, map[string]any{"key_id": "test-key", "key": key})
	case request.Method == http.MethodPut && request.URL.Path == prefix+"/actions/secrets/"+ProviderSecretName:
		h.putCount++
		var body struct {
			KeyID          string `json:"key_id"`
			EncryptedValue string `json:"encrypted_value"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			h.t.Errorf("decode secret request: %v", err)
		}
		if body.KeyID != "test-key" {
			h.t.Errorf("key ID = %q", body.KeyID)
		}
		sealed, err := base64.StdEncoding.DecodeString(body.EncryptedValue)
		if err != nil {
			h.t.Errorf("decode encrypted secret: %v", err)
		}
		opened, ok := box.OpenAnonymous(nil, sealed, h.publicKey, h.privateKey)
		if !ok {
			h.t.Error("encrypted provider secret is not a libsodium-compatible sealed box")
		}
		h.opened = append([]byte(nil), opened...)
		zero(opened)
		zero(sealed)
		if h.putStatus != 0 {
			if h.persistOnPutError {
				h.exists = true
			}
			h.writeJSON(response, h.putStatus, map[string]any{"message": "provider secret create failed"})
			return
		}
		h.exists = true
		response.WriteHeader(http.StatusCreated)
	default:
		h.t.Errorf("unexpected API request: %s %s", request.Method, request.URL.Path)
		h.writeJSON(response, http.StatusNotFound, map[string]any{"message": "unexpected request"})
	}
}

func (h *providerSecretHarness) writeJSON(response http.ResponseWriter, status int, value any) {
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		h.t.Errorf("encode response: %v", err)
	}
}
