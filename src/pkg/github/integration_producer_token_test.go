package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type integrationProducerTransport func(*http.Request) (*http.Response, error)

const producerIdentityPayload = `{"version":1,"fixture":"producer identity, not CI success"}`

func (f integrationProducerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func producerIdentityFixture(t *testing.T) (*rsa.PrivateKey, IntegrationProducerBinding, jwt.MapClaims) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, integrationProducerMinRSABits)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately historical: this attests durable evidence, not bearer-token
	// login or a decision that current eligibility is still satisfied.
	issued := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	binding := IntegrationProducerBinding{
		Repo: "owner/repo", RepositoryID: 1, ActorID: 2, RunID: 3, RunAttempt: 1, FinalizerCheckRunID: 4,
		WorkflowPath: ".github/workflows/integration-candidate.yml",
		WorkflowSHA:  strings.Repeat("a", 40), SubjectID: strings.Repeat("b", integrationProducerDigestBytes),
		PayloadSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(producerIdentityPayload))),
		JobStartedAt:  issued.Add(-time.Minute), JobCompletedAt: issued.Add(time.Minute), ObservedAt: issued.Add(time.Hour),
	}
	ref := integrationProducerRefPrefix + binding.SubjectID
	return key, binding, jwt.MapClaims{
		"iss": integrationProducerIssuer, "aud": integrationProducerAudience + binding.PayloadSHA256,
		"sub": "repo:owner/repo:ref:" + ref, "iat": issued.Unix(), "nbf": issued.Unix(), "exp": issued.Add(time.Minute).Unix(),
		"repository": binding.Repo, "repository_id": "1", "actor_id": "2", "run_id": "3", "run_attempt": "1", "check_run_id": "4",
		"workflow_ref": binding.Repo + "/" + binding.WorkflowPath + "@" + ref, "workflow_sha": binding.WorkflowSHA,
		"ref": ref, "ref_type": "tag", "event_name": "workflow_dispatch", "runner_environment": "github-hosted",
	}
}

func signProducerToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "github-key"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestIntegrationProducerAuthenticatesExactHistoricalFinalizer(t *testing.T) {
	key, binding, claims := producerIdentityFixture(t)
	verifier := NewIntegrationProducerVerifier()
	verifier.keys, verifier.refreshedAt = map[string]*rsa.PublicKey{"github-key": &key.PublicKey}, time.Now()
	for _, checkID := range []any{"4", json.Number("4")} {
		claims["check_run_id"] = checkID
		identity, err := verifier.Verify(context.Background(), signProducerToken(t, key, claims), []byte(producerIdentityPayload), binding)
		if err != nil || identity.FinalizerCheckRunID != binding.FinalizerCheckRunID ||
			identity.PayloadSHA256 != binding.PayloadSHA256 || identity.KeyID != "github-key" ||
			len(identity.SigningKeySPKI) == 0 || !integrationProducerDigest(identity.SigningKeySHA256) ||
			identity.VerifiedAt != binding.ObservedAt {
			t.Fatalf("exact historical producer: %+v %v", identity, err)
		}
	}
}

func TestIntegrationMaterializerHasDistinctRoleRefAndInstalledTransport(t *testing.T) {
	key, candidate, claims := producerIdentityFixture(t)
	candidate.WorkflowPath = ".github/workflows/integration-candidate-prepare.yml"
	binding := IntegrationMaterializerBinding{
		IntegrationProducerBinding: candidate, DefaultBranch: "v5", EventName: "repository_dispatch",
	}
	claims["aud"] = integrationMaterializerAudience + binding.PayloadSHA256
	claims["ref"], claims["ref_type"] = "refs/heads/v5", "branch"
	claims["sub"] = "repo:owner/repo:ref:refs/heads/v5"
	claims["workflow_ref"] = binding.Repo + "/" + binding.WorkflowPath + "@refs/heads/v5"
	verifier := NewIntegrationProducerVerifier()
	verifier.keys, verifier.refreshedAt = map[string]*rsa.PublicKey{"github-key": &key.PublicKey}, time.Now()
	for _, event := range []string{"workflow_dispatch", "repository_dispatch"} {
		binding.EventName, claims["event_name"] = event, event
		raw := signProducerToken(t, key, claims)
		got, err := verifier.VerifyMaterializer(context.Background(), raw, []byte(producerIdentityPayload), binding)
		if err != nil || got.Role != "materializer" {
			t.Fatalf("installed %s materializer: %+v %v", event, got, err)
		}
		if _, err := verifier.Verify(context.Background(), raw, []byte(producerIdentityPayload), candidate); err == nil {
			t.Fatal("materialization identity became candidate CI identity")
		}
	}
	for _, bad := range []IntegrationMaterializerBinding{
		{IntegrationProducerBinding: candidate, DefaultBranch: "v5", EventName: "pull_request"},
		{IntegrationProducerBinding: candidate, DefaultBranch: "../v5", EventName: "repository_dispatch"},
		{IntegrationProducerBinding: candidate, DefaultBranch: "v6", EventName: "repository_dispatch"},
		{IntegrationProducerBinding: candidate, DefaultBranch: "v5.lock", EventName: "repository_dispatch"},
		{IntegrationProducerBinding: candidate, DefaultBranch: "v5", EventName: "workflow_dispatch"},
	} {
		if _, err := verifier.VerifyMaterializer(context.Background(), signProducerToken(t, key, claims), []byte(producerIdentityPayload), bad); err == nil {
			t.Fatalf("unsupported/ref-mismatched materializer accepted: %+v", bad)
		}
	}
}

func TestIntegrationProducerRejectsCrossSubjectRunAndJobClaims(t *testing.T) {
	key, binding, original := producerIdentityFixture(t)
	verifier := NewIntegrationProducerVerifier()
	verifier.keys, verifier.refreshedAt = map[string]*rsa.PublicKey{"github-key": &key.PublicKey}, time.Now()
	for _, test := range []struct {
		name  string
		claim string
		value any
	}{
		{"issuer", "iss", "https://attacker.invalid"},
		{"payload", "aud", integrationProducerAudience + strings.Repeat("d", integrationProducerDigestBytes)},
		{"extra-audience", "aud", []string{integrationProducerAudience + binding.PayloadSHA256, "another-audience"}},
		{"repository", "repository", "other/repo"},
		{"repository-id", "repository_id", "9"},
		{"actor", "actor_id", "9"},
		{"run", "run_id", "9"},
		{"attempt", "run_attempt", "2"},
		{"untrusted-job", "check_run_id", "9"},
		{"missing-job", "check_run_id", nil},
		{"fractional-job", "check_run_id", 4.5},
		{"exponent-job", "check_run_id", json.Number("4e0")},
		{"workflow", "workflow_ref", "owner/repo/.github/workflows/untrusted.yml@" + integrationProducerRefPrefix + binding.SubjectID},
		{"workflow-sha", "workflow_sha", strings.Repeat("d", 40)},
		{"ref", "ref", "refs/heads/v5"},
		{"ref-type", "ref_type", "branch"},
		{"event", "event_name", "pull_request"},
		{"runner", "runner_environment", "self-hosted"},
		{"environment", "environment", "production"},
		{"missing-issuance", "iat", nil},
		{"missing-not-before", "nbf", nil},
		{"missing-expiry", "exp", nil},
		{"early-issuance", "iat", binding.JobStartedAt.Add(-time.Hour).Unix()},
		{"late-issuance", "iat", binding.JobCompletedAt.Add(time.Hour).Unix()},
		{"not-yet-valid", "nbf", binding.JobCompletedAt.Add(time.Hour).Unix()},
		{"unbounded-not-before", "nbf", binding.JobStartedAt.Add(-time.Hour).Unix()},
		{"expired-at-issuance", "exp", binding.JobStartedAt.Unix()},
		{"excessive-lifetime", "exp", binding.JobCompletedAt.Add(integrationProducerMaxLifetime).Unix()},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := make(jwt.MapClaims, len(original))
			for name, value := range original {
				claims[name] = value
			}
			claims[test.claim] = test.value
			if _, err := verifier.Verify(context.Background(), signProducerToken(t, key, claims), []byte(producerIdentityPayload), binding); err == nil {
				t.Fatal("accepted a different, unbound or temporally invalid producer")
			}
		})
	}
}

func TestIntegrationProducerRejectsUntrustedSignatureAndAlgorithms(t *testing.T) {
	key, binding, claims := producerIdentityFixture(t)
	verifier := NewIntegrationProducerVerifier()
	verifier.keys, verifier.refreshedAt = map[string]*rsa.PublicKey{"github-key": &key.PublicKey}, time.Now()
	other, err := rsa.GenerateKey(rand.Reader, integrationProducerMinRSABits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), signProducerToken(t, other, claims), []byte(producerIdentityPayload), binding); err == nil {
		t.Fatal("forged producer signature accepted")
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = "github-key"
	raw, err := token.SignedString([]byte("not-a-github-signing-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), raw, []byte(producerIdentityPayload), binding); err == nil {
		t.Fatal("RSA/HMAC algorithm confusion accepted")
	}
	if _, err := verifier.Verify(context.Background(), strings.Repeat("x", integrationProducerMaxToken+1), []byte(producerIdentityPayload), binding); err == nil {
		t.Fatal("unbounded token accepted")
	}
}

func producerJWKS(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	wire := map[string]string{
		"kid": "github-key", "kty": "RSA", "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
	body, err := json.Marshal(map[string]any{"keys": []map[string]string{wire}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestIntegrationProducerUsesOnlyBoundedUnauthenticatedIssuerKeys(t *testing.T) {
	key, binding, claims := producerIdentityFixture(t)
	raw, body := signProducerToken(t, key, claims), producerJWKS(t, key)
	verifier := NewIntegrationProducerVerifier()
	calls := atomic.Int32{}
	entered, release := make(chan struct{}), make(chan struct{})
	verifier.client.Transport = integrationProducerTransport(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.String() != integrationProducerJWKS || request.Header.Get("Authorization") != "" {
			t.Error("issuer transport leaked authority or trusted a token-selected URL")
		}
		close(entered)
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	const parallelRequests = 8
	var waiting sync.WaitGroup
	verify := func() {
		defer waiting.Done()
		if _, err := verifier.Verify(context.Background(), raw, []byte(producerIdentityPayload), binding); err != nil {
			t.Error(err)
		}
	}
	waiting.Add(1)
	go verify()
	<-entered
	if !verifier.mu.TryLock() {
		close(release)
		waiting.Wait()
		t.Fatal("remote issuer I/O held the cache mutex")
	}
	verifier.mu.Unlock()
	for range parallelRequests - 1 {
		waiting.Add(1)
		go verify()
	}
	close(release)
	waiting.Wait()
	if calls.Load() != 1 {
		t.Fatalf("parallel observation caused issuer poll fanout: %d", calls.Load())
	}
	for range parallelRequests {
		if _, err := verifier.key(context.Background(), "unknown-key"); err == nil {
			t.Fatal("unknown key accepted")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unknown-key flood escaped refresh bounds")
	}
}

func TestIntegrationProducerIssuerFailuresNeverBecomeIdentity(t *testing.T) {
	key, binding, claims := producerIdentityFixture(t)
	for _, body := range []string{
		`invalid`, `{"keys":[]}`, `{"keys":[{"kid":"k","kty":"EC","use":"sig","alg":"ES256"}]}`,
		`{"keys":[{"kid":"k","kty":"RSA","use":"sig","alg":"RS256","n":"AA","e":"Aw"}]}`,
		strings.Repeat(" ", integrationProducerMaxKeyBytes+1),
	} {
		verifier := NewIntegrationProducerVerifier()
		verifier.client.Transport = integrationProducerTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if _, err := verifier.Verify(context.Background(), signProducerToken(t, key, claims), []byte(producerIdentityPayload), binding); err == nil {
			t.Fatal("missing or malformed issuer keys became trusted identity")
		}
	}
	verifier := NewIntegrationProducerVerifier()
	verifier.client.Transport = integrationProducerTransport(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("issuer unavailable")
	})
	if _, err := verifier.Verify(context.Background(), signProducerToken(t, key, claims), []byte(producerIdentityPayload), binding); err == nil {
		t.Fatal("unavailable issuer became trusted identity")
	}
}

func TestIntegrationProducerInvalidBindingsDoNotFetchKeys(t *testing.T) {
	_, binding, _ := producerIdentityFixture(t)
	verifier := NewIntegrationProducerVerifier()
	verifier.client.Transport = integrationProducerTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid binding performed remote I/O")
		return nil, fmt.Errorf("unexpected network")
	})
	for _, change := range []func(*IntegrationProducerBinding){
		func(b *IntegrationProducerBinding) { b.SubjectID = "" },
		func(b *IntegrationProducerBinding) { b.PayloadSHA256 = "" },
		func(b *IntegrationProducerBinding) { b.WorkflowSHA = "short" },
		func(b *IntegrationProducerBinding) { b.RepositoryID = 0 },
		func(b *IntegrationProducerBinding) { b.FinalizerCheckRunID = 0 },
		func(b *IntegrationProducerBinding) { b.Repo = "/repo" },
		func(b *IntegrationProducerBinding) { b.WorkflowPath = "../untrusted.yml" },
		func(b *IntegrationProducerBinding) { b.JobCompletedAt = b.JobStartedAt.Add(-time.Minute) },
		func(b *IntegrationProducerBinding) { b.ObservedAt = time.Time{} },
	} {
		bad := binding
		change(&bad)
		if _, err := verifier.Verify(context.Background(), "invalid", []byte(producerIdentityPayload), bad); err == nil {
			t.Fatal("invalid binding accepted")
		}
		for _, payload := range [][]byte{nil, []byte("replaced manifest"), make([]byte, integrationProducerMaxPayload+1)} {
			if _, err := verifier.Verify(context.Background(), "token", payload, binding); err == nil {
				t.Fatal("missing, substituted or unbounded payload accepted")
			}
		}
	}
	if _, err := verifier.Verify(context.Background(), "", []byte(producerIdentityPayload), binding); err == nil {
		t.Fatal("missing token accepted")
	}
	var missing *IntegrationProducerVerifier
	if _, err := missing.Verify(context.Background(), "token", []byte(producerIdentityPayload), binding); err == nil {
		t.Fatal("missing verifier accepted")
	}
}
