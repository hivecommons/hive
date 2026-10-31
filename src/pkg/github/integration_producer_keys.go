package github

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	integrationProducerMaxKeyID     = 128
	integrationProducerMaxKeys      = 64
	integrationProducerMaxKeyBytes  = 256 << 10
	integrationProducerMinRSABits   = 2048
	integrationProducerMaxRSABits   = 8192
	integrationProducerMaxExponent  = 1<<31 - 1
	integrationProducerKeyTTL       = time.Hour
	integrationProducerRefreshDelay = time.Minute
	integrationProducerHTTPTimeout  = 10 * time.Second
	integrationProducerJWKS         = integrationProducerIssuer + "/.well-known/jwks"
)

// IntegrationProducerVerifier caches only public keys from GitHub's fixed
// issuer. It never uses the forge client or sends an installation credential.
// Construction performs no I/O; disabled execution need not construct it.
type IntegrationProducerVerifier struct {
	mu          sync.Mutex
	client      *http.Client
	keys        map[string]*rsa.PublicKey
	refreshedAt time.Time
	attemptedAt time.Time
	refreshing  chan struct{}
}

func NewIntegrationProducerVerifier() *IntegrationProducerVerifier {
	return &IntegrationProducerVerifier{client: &http.Client{
		Timeout: integrationProducerHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (v *IntegrationProducerVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	for {
		now := time.Now()
		v.mu.Lock()
		key := v.keys[kid]
		if key != nil && now.Sub(v.refreshedAt) < integrationProducerKeyTTL {
			v.mu.Unlock()
			return key, nil
		}
		if pending := v.refreshing; pending != nil {
			v.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-pending:
				continue
			}
		}
		if now.Sub(v.attemptedAt) < integrationProducerRefreshDelay {
			v.mu.Unlock()
			return nil, fmt.Errorf("integration producer key unavailable; issuer refresh bounded")
		}
		v.attemptedAt = now
		v.refreshing = make(chan struct{})
		v.mu.Unlock()

		keys, err := v.fetch(ctx)
		v.mu.Lock()
		if err == nil {
			v.keys, v.refreshedAt = keys, time.Now()
		}
		close(v.refreshing)
		v.refreshing = nil
		v.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

func (v *IntegrationProducerVerifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	if v.client == nil {
		return nil, fmt.Errorf("integration producer issuer transport unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, integrationProducerJWKS, nil)
	if err != nil {
		return nil, err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("integration producer issuer returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, integrationProducerMaxKeyBytes+1))
	if err != nil || len(body) > integrationProducerMaxKeyBytes {
		return nil, fmt.Errorf("integration producer key response unavailable or oversized")
	}
	var document struct {
		Keys []struct {
			KID string `json:"kid"`
			KTY string `json:"kty"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil || len(document.Keys) > integrationProducerMaxKeys {
		return nil, fmt.Errorf("integration producer signing key set invalid")
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, wire := range document.Keys {
		if wire.KTY != "RSA" || wire.Use != "sig" || wire.Alg != "RS256" {
			continue
		}
		if wire.KID == "" || len(wire.KID) > integrationProducerMaxKeyID || keys[wire.KID] != nil {
			return nil, fmt.Errorf("integration producer signing key identity invalid or duplicated")
		}
		modulus, nerr := base64.RawURLEncoding.DecodeString(wire.N)
		exponent, eerr := base64.RawURLEncoding.DecodeString(wire.E)
		n, e := new(big.Int).SetBytes(modulus), new(big.Int).SetBytes(exponent)
		if !e.IsInt64() {
			return nil, fmt.Errorf("integration producer RSA exponent invalid")
		}
		exponentValue := e.Int64()
		if nerr != nil || eerr != nil || n.BitLen() < integrationProducerMinRSABits ||
			n.BitLen() > integrationProducerMaxRSABits || exponentValue < 3 ||
			exponentValue > integrationProducerMaxExponent || e.Bit(0) == 0 {
			return nil, fmt.Errorf("integration producer RSA signing key invalid")
		}
		keys[wire.KID] = &rsa.PublicKey{N: n, E: int(exponentValue)}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("integration producer issuer supplied no usable signing keys")
	}
	return keys, nil
}
