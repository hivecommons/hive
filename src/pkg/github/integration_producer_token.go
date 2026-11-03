package github

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	integrationProducerIssuer           = "https://token.actions.githubusercontent.com"
	integrationProducerAudience         = "hive-integration-producer-v1/"
	integrationMaterializerAudience     = "hive-integration-materializer-v1/"
	integrationProducerRefPrefix        = "refs/tags/hive-integration/"
	integrationProducerMaxToken         = 16 << 10
	integrationProducerMaxPayload       = 256 << 10
	integrationProducerClockSkew        = 2 * time.Minute
	integrationProducerMaxLifetime      = time.Hour
	integrationProducerDigestBytes      = 64
	integrationProducerMaxDefaultBranch = 255
)

// IntegrationProducerBinding must come from the enrolled policy, exact
// candidate and authoritative run/attempt/job observations, not the artifact.
type IntegrationProducerBinding struct {
	Repo                string
	RepositoryID        int64
	ActorID             int64
	RunID               int64
	RunAttempt          int64
	FinalizerCheckRunID int64
	WorkflowPath        string
	WorkflowSHA         string
	SubjectID           string
	PayloadSHA256       string
	JobStartedAt        time.Time
	JobCompletedAt      time.Time
	ObservedAt          time.Time
}

// IntegrationProducerIdentity authenticates a manifest's producer only. It is
// not passing CI, current eligibility, promotion permission or a capacity grant.
type IntegrationProducerIdentity struct {
	Role                string
	Issuer              string
	KeyID               string
	IssuedAt            time.Time
	RunID               int64
	RunAttempt          int64
	FinalizerCheckRunID int64
	PayloadSHA256       string
	SigningKeySPKI      []byte `json:"-"`
	SigningKeySHA256    string
	VerifiedAt          time.Time
}

// IntegrationMaterializerBinding describes the independently observed trusted
// default-branch finalizer. SubjectID is the exact preparation request digest.
// EventName is the explicitly installed transport, never an API-error fallback.
type IntegrationMaterializerBinding struct {
	IntegrationProducerBinding
	DefaultBranch string
	EventName     string
}

type integrationProducerProfile struct {
	role, audience, ref, refType, event string
}

type integrationProducerClaims struct {
	jwt.RegisteredClaims
	Repository        string          `json:"repository"`
	RepositoryID      json.RawMessage `json:"repository_id"`
	ActorID           json.RawMessage `json:"actor_id"`
	RunID             json.RawMessage `json:"run_id"`
	RunAttempt        json.RawMessage `json:"run_attempt"`
	CheckRunID        json.RawMessage `json:"check_run_id"`
	WorkflowRef       string          `json:"workflow_ref"`
	WorkflowSHA       string          `json:"workflow_sha"`
	Ref               string          `json:"ref"`
	RefType           string          `json:"ref_type"`
	EventName         string          `json:"event_name"`
	RunnerEnvironment string          `json:"runner_environment"`
	Environment       string          `json:"environment"`
}

func integrationProducerDigest(value string) bool {
	return len(value) == integrationProducerDigestBytes && strings.Trim(value, "0123456789abcdef") == "" &&
		strings.Trim(value, "0") != ""
}

func (b IntegrationProducerBinding) validate() error {
	parts := strings.Split(b.Repo, "/")
	if !integrationProducerDigest(b.SubjectID) || !integrationProducerDigest(b.PayloadSHA256) ||
		!integrationOID(b.WorkflowSHA) || b.WorkflowSHA != strings.ToLower(b.WorkflowSHA) ||
		strings.Trim(b.WorkflowSHA, "0") == "" ||
		b.RepositoryID <= 0 || b.ActorID <= 0 || b.RunID <= 0 || b.RunAttempt <= 0 || b.FinalizerCheckRunID <= 0 ||
		len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(b.Repo, "?#%\\ \t\r\n") ||
		!strings.HasPrefix(b.WorkflowPath, ".github/workflows/") ||
		strings.Contains(b.WorkflowPath, "..") || strings.ContainsAny(b.WorkflowPath, "@ \t\r\n") ||
		!strings.HasSuffix(b.WorkflowPath, ".yml") ||
		b.JobStartedAt.IsZero() || b.JobCompletedAt.Before(b.JobStartedAt) ||
		b.ObservedAt.IsZero() || b.JobCompletedAt.After(b.ObservedAt.Add(integrationProducerClockSkew)) {
		return fmt.Errorf("incomplete authoritative integration producer binding")
	}
	return nil
}

func integrationProducerID(raw json.RawMessage, expected int64) bool {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return false
		}
		text = string(number)
	}
	return text == strconv.FormatInt(expected, 10)
}

// Verify authenticates the exact payload digest using GitHub's fixed issuer.
// JWT expiry is not a permanent evidence expiry: the signed issuance must fall
// inside the independently observed finalizer interval. Live eligibility and
// evidence freshness must still be checked separately before any promotion.
func (v *IntegrationProducerVerifier) Verify(ctx context.Context, raw string, payload []byte, binding IntegrationProducerBinding) (IntegrationProducerIdentity, error) {
	return v.verify(ctx, raw, payload, binding, integrationProducerProfile{
		role: "candidate", audience: integrationProducerAudience,
		ref: integrationProducerRefPrefix + binding.SubjectID, refType: "tag", event: "workflow_dispatch",
	})
}

// VerifyMaterializer authenticates a trusted materialization receipt, not
// candidate CI success, shared mutation ownership or promotion permission.
func (v *IntegrationProducerVerifier) VerifyMaterializer(ctx context.Context, raw string, payload []byte, binding IntegrationMaterializerBinding) (IntegrationProducerIdentity, error) {
	branch := binding.DefaultBranch
	if branch == "" || branch == "@" || len(branch) > integrationProducerMaxDefaultBranch || strings.Contains(branch, "..") ||
		strings.Contains(branch, "@{") || strings.ContainsAny(branch, "\x00\x7f ~^:?*[\\\t\r\n") {
		return IntegrationProducerIdentity{}, fmt.Errorf("authoritative materializer default branch unavailable")
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return IntegrationProducerIdentity{}, fmt.Errorf("authoritative materializer default branch invalid")
		}
	}
	if binding.EventName != "workflow_dispatch" && binding.EventName != "repository_dispatch" {
		return IntegrationProducerIdentity{}, fmt.Errorf("materializer transport is not explicitly supported")
	}
	return v.verify(ctx, raw, payload, binding.IntegrationProducerBinding, integrationProducerProfile{
		role: "materializer", audience: integrationMaterializerAudience,
		ref: "refs/heads/" + branch, refType: "branch", event: binding.EventName,
	})
}

func (v *IntegrationProducerVerifier) verify(ctx context.Context, raw string, payload []byte, binding IntegrationProducerBinding, profile integrationProducerProfile) (IntegrationProducerIdentity, error) {
	if err := binding.validate(); err != nil {
		return IntegrationProducerIdentity{}, err
	}
	if len(payload) == 0 || len(payload) > integrationProducerMaxPayload {
		return IntegrationProducerIdentity{}, fmt.Errorf("integration producer payload unavailable or oversized")
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != binding.PayloadSHA256 {
		return IntegrationProducerIdentity{}, fmt.Errorf("integration producer payload does not match the bound digest")
	}
	if v == nil || raw == "" || len(raw) > integrationProducerMaxToken {
		return IntegrationProducerIdentity{}, fmt.Errorf("integration producer token unavailable or oversized")
	}
	claims := new(integrationProducerClaims)
	keyID := ""
	var publicKey []byte
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" || len(kid) > integrationProducerMaxKeyID {
			return nil, fmt.Errorf("integration producer signing key identity unavailable")
		}
		keyID = kid
		key, err := v.key(ctx, kid)
		if err != nil {
			return nil, err
		}
		publicKey, err = x509.MarshalPKIXPublicKey(key)
		if err != nil {
			return nil, err
		}
		return key, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}), jwt.WithoutClaimsValidation())
	if err != nil {
		return IntegrationProducerIdentity{}, fmt.Errorf("integration producer signature rejected: %w", err)
	}
	if token == nil || !token.Valid {
		return IntegrationProducerIdentity{}, fmt.Errorf("integration producer signature unavailable")
	}
	if claims.Issuer != integrationProducerIssuer || len(claims.Audience) != 1 ||
		claims.Audience[0] != profile.audience+binding.PayloadSHA256 ||
		claims.Repository != binding.Repo || !integrationProducerID(claims.RepositoryID, binding.RepositoryID) ||
		!integrationProducerID(claims.ActorID, binding.ActorID) || !integrationProducerID(claims.RunID, binding.RunID) ||
		!integrationProducerID(claims.RunAttempt, binding.RunAttempt) ||
		!integrationProducerID(claims.CheckRunID, binding.FinalizerCheckRunID) ||
		claims.WorkflowRef != binding.Repo+"/"+binding.WorkflowPath+"@"+profile.ref ||
		claims.WorkflowSHA != binding.WorkflowSHA || claims.Ref != profile.ref || claims.RefType != profile.refType ||
		claims.EventName != profile.event || claims.RunnerEnvironment != "github-hosted" || claims.Environment != "" {
		return IntegrationProducerIdentity{}, fmt.Errorf("integration producer identity does not match the authoritative candidate finalizer")
	}
	if claims.IssuedAt == nil || claims.NotBefore == nil || claims.ExpiresAt == nil ||
		claims.IssuedAt.Before(binding.JobStartedAt.Add(-integrationProducerClockSkew)) ||
		claims.IssuedAt.After(binding.JobCompletedAt.Add(integrationProducerClockSkew)) ||
		claims.NotBefore.Before(claims.IssuedAt.Add(-integrationProducerClockSkew)) ||
		claims.NotBefore.After(claims.IssuedAt.Add(integrationProducerClockSkew)) ||
		!claims.ExpiresAt.After(claims.IssuedAt.Time) ||
		claims.ExpiresAt.Sub(claims.IssuedAt.Time) > integrationProducerMaxLifetime {
		return IntegrationProducerIdentity{}, fmt.Errorf("integration producer issuance is outside the authoritative finalizer interval")
	}
	keyDigest := sha256.Sum256(publicKey)
	return IntegrationProducerIdentity{
		Role:   profile.role,
		Issuer: claims.Issuer, KeyID: keyID, IssuedAt: claims.IssuedAt.Time,
		RunID: binding.RunID, RunAttempt: binding.RunAttempt, FinalizerCheckRunID: binding.FinalizerCheckRunID,
		PayloadSHA256:  binding.PayloadSHA256,
		SigningKeySPKI: publicKey, SigningKeySHA256: hex.EncodeToString(keyDigest[:]), VerifiedAt: binding.ObservedAt,
	}, nil
}
