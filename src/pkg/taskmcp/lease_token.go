package taskmcp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const LeaseTokenPrefix = "hive_mcp_v1"

var (
	ErrLeaseTokenInvalid = errors.New("invalid task MCP lease token")
	ErrLeaseTokenExpired = errors.New("expired task MCP lease token")
)

type LeaseTokenClaims struct {
	ID          string    `json:"jti"`
	TaskID      string    `json:"task_id"`
	Identity    string    `json:"identity"`
	Repo        string    `json:"repo"`
	Number      int       `json:"number,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	IssuedAt    time.Time `json:"issued_at"`
	Contributor string    `json:"contributor,omitempty"`
}

func MintLeaseToken(secret []byte, claims LeaseTokenClaims, now time.Time) (string, LeaseTokenClaims, error) {
	if len(secret) == 0 || claims.TaskID == "" || claims.Identity == "" || claims.Repo == "" || claims.ExpiresAt.IsZero() {
		return "", LeaseTokenClaims{}, ErrLeaseTokenInvalid
	}
	if claims.ID == "" {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", LeaseTokenClaims{}, err
		}
		claims.ID = base64.RawURLEncoding.EncodeToString(raw[:])
	}
	if now.IsZero() {
		now = time.Now()
	}
	claims.IssuedAt = now.UTC()
	claims.ExpiresAt = claims.ExpiresAt.UTC()
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", LeaseTokenClaims{}, err
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := leaseTokenSignature(secret, payloadB64)
	return LeaseTokenPrefix + "." + payloadB64 + "." + sig, claims, nil
}

func VerifyLeaseToken(secret []byte, token string, now time.Time) (LeaseTokenClaims, error) {
	if len(secret) == 0 {
		return LeaseTokenClaims{}, ErrLeaseTokenInvalid
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] != LeaseTokenPrefix {
		return LeaseTokenClaims{}, ErrLeaseTokenInvalid
	}
	want := leaseTokenSignature(secret, parts[1])
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return LeaseTokenClaims{}, ErrLeaseTokenInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return LeaseTokenClaims{}, ErrLeaseTokenInvalid
	}
	var claims LeaseTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return LeaseTokenClaims{}, ErrLeaseTokenInvalid
	}
	if claims.ID == "" || claims.TaskID == "" || claims.Identity == "" || claims.Repo == "" || claims.ExpiresAt.IsZero() {
		return LeaseTokenClaims{}, ErrLeaseTokenInvalid
	}
	if now.IsZero() {
		now = time.Now()
	}
	if !now.Before(claims.ExpiresAt) {
		return LeaseTokenClaims{}, ErrLeaseTokenExpired
	}
	return claims, nil
}

func HashLeaseToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (c LeaseTokenClaims) Matches(taskID, repo string, number int) bool {
	return c.TaskID == taskID && strings.EqualFold(c.Repo, repo) && (number == 0 || c.Number == 0 || c.Number == number)
}

func leaseTokenSignature(secret []byte, payloadB64 string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(LeaseTokenPrefix + "." + payloadB64))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func LeaseTokenWrongScopeError(taskID, repo string, number int) error {
	if number > 0 {
		return fmt.Errorf("%w: lease does not cover %s %s#%d", ErrForbidden, taskID, repo, number)
	}
	return fmt.Errorf("%w: lease does not cover %s %s", ErrForbidden, taskID, repo)
}
