package integration

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hivecommons/hive/pkg/forgestate"
)

const (
	executionArchiveMaxPayload  = 256 << 10
	executionArchiveMaxToken    = 16 << 10
	executionArchiveIssuer      = "https://token.actions.githubusercontent.com"
	executionArchiveSkew        = 2 * time.Minute
	executionArchiveMaxLifetime = time.Hour
	executionArchiveMaxKey      = 16 << 10
	executionArchiveMaxChecks   = 100
)

func archivedID(value any, expected int64) bool {
	switch typed := value.(type) {
	case string:
		return typed == strconv.FormatInt(expected, 10)
	case json.Number:
		return typed.String() == strconv.FormatInt(expected, 10)
	default:
		return false
	}
}

// validateArchivedIdentity rechecks already validated private history without
// fetching a current JWKS (which may have retired the historical key). It never
// upgrades arbitrary caller-supplied keys into new trusted issuer observations.
func validateArchivedIdentity(a ExecutionArchive, repo string, runID int64, attempt int) error {
	if a.Role != "candidate" && a.Role != "materializer" || a.Issuer != executionArchiveIssuer ||
		a.KeyID == "" || a.VerifiedAt.IsZero() || len(a.Payload) == 0 || len(a.Payload) > executionArchiveMaxPayload ||
		len(a.Token) == 0 || len(a.Token) > executionArchiveMaxToken || a.ArtifactID <= 0 ||
		a.WorkflowID <= 0 || a.SuiteID <= 0 || len(a.CheckIDs) == 0 || len(a.CheckIDs) > executionArchiveMaxChecks || a.FinalizerCheckID <= 0 ||
		len(a.SigningKeySPKI) == 0 || len(a.SigningKeySPKI) > executionArchiveMaxKey ||
		a.RepositoryID <= 0 || a.ActorID <= 0 || a.JobStartedAt.IsZero() || a.JobCompletedAt.Before(a.JobStartedAt) ||
		!fullSHA(a.WorkflowSHA) || a.WorkflowPath == "" || a.Ref == "" || a.Event == "" {
		return fmt.Errorf("incomplete private verified producer history")
	}
	checks := map[int64]bool{}
	for _, id := range a.CheckIDs {
		if id <= 0 || checks[id] {
			return fmt.Errorf("archived check cohort invalid/duplicated")
		}
		checks[id] = true
	}
	if !checks[a.FinalizerCheckID] {
		return fmt.Errorf("archived finalizer absent from actual check cohort")
	}
	keyDigest := sha256.Sum256(a.SigningKeySPKI)
	if hex.EncodeToString(keyDigest[:]) != a.SigningKeySHA256 {
		return fmt.Errorf("archived signer key digest changed")
	}
	parsed, err := x509.ParsePKIXPublicKey(a.SigningKeySPKI)
	if err != nil {
		return err
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok || key.N.BitLen() < 2048 || key.N.BitLen() > 8192 {
		return fmt.Errorf("unsupported archived producer key")
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(a.Token, claims, func(token *jwt.Token) (any, error) {
		if token.Header["kid"] != a.KeyID {
			return nil, fmt.Errorf("archived issuer key identity changed")
		}
		return key, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}), jwt.WithoutClaimsValidation(), jwt.WithJSONNumber())
	if err != nil || token == nil || !token.Valid {
		return fmt.Errorf("archived producer signature invalid: %v", err)
	}
	payloadDigest := sha256.Sum256(a.Payload)
	audience := "hive-integration-producer-v1/"
	if a.Role == "materializer" {
		audience = "hive-integration-materializer-v1/"
	}
	audiences, err := claims.GetAudience()
	if err != nil || len(audiences) != 1 || audiences[0] != audience+hex.EncodeToString(payloadDigest[:]) ||
		claims["iss"] != a.Issuer || claims["repository"] != repo ||
		!archivedID(claims["repository_id"], a.RepositoryID) || !archivedID(claims["actor_id"], a.ActorID) ||
		!archivedID(claims["run_id"], runID) || !archivedID(claims["run_attempt"], int64(attempt)) ||
		!archivedID(claims["check_run_id"], a.FinalizerCheckID) ||
		claims["workflow_ref"] != repo+"/"+a.WorkflowPath+"@"+a.Ref || claims["workflow_sha"] != a.WorkflowSHA ||
		claims["ref"] != a.Ref || claims["event_name"] != a.Event || claims["runner_environment"] != "github-hosted" ||
		claims["environment"] != nil && claims["environment"] != "" {
		return fmt.Errorf("archived producer identity/digest binding changed")
	}
	issued, err := claims.GetIssuedAt()
	if err != nil || issued == nil || issued.Before(a.JobStartedAt.Add(-executionArchiveSkew)) ||
		issued.After(a.JobCompletedAt.Add(executionArchiveSkew)) {
		return fmt.Errorf("archived signed issuance outside actual finalizer interval")
	}
	notBefore, nbfErr := claims.GetNotBefore()
	expires, expErr := claims.GetExpirationTime()
	if nbfErr != nil || expErr != nil || notBefore == nil || expires == nil ||
		notBefore.Before(issued.Add(-executionArchiveSkew)) || notBefore.After(issued.Add(executionArchiveSkew)) ||
		!expires.After(issued.Time) || expires.Sub(issued.Time) > executionArchiveMaxLifetime {
		return fmt.Errorf("archived historical token lifetime changed")
	}
	return nil
}

func validateExecutionArchive(proof ExecutionProof) error {
	a := *proof.Archive
	if a.Role != "candidate" || a.WorkflowSHA != proof.Subject.Prefixes[len(proof.Subject.Prefixes)-1].Commit ||
		a.Ref != "refs/tags/hive-integration/"+proof.Subject.ID || a.Event != "workflow_dispatch" {
		return fmt.Errorf("archived candidate role/subject binding changed")
	}
	return validateExecutionArchiveBody(proof)
}

func validateMaterializationArchive(plan ExecutionPlan) error {
	var receipt struct {
		Schema   int    `json:"schema"`
		Protocol string `json:"protocol"`
		Request  struct {
			Schema     int                                     `json:"schema"`
			Protocol   string                                  `json:"protocol"`
			Engine     string                                  `json:"engine"`
			Repo       string                                  `json:"repo"`
			Base       string                                  `json:"base"`
			BaseSHA    string                                  `json:"baseSHA"`
			Policy     string                                  `json:"policy"`
			Generation int64                                   `json:"generation"`
			Members    []forgestate.IntegrationCandidateMember `json:"members"`
			CreatedAt  string                                  `json:"createdAt"`
		} `json:"request"`
		Prepared []struct {
			Subject     forgestate.IntegrationCandidateSubject `json:"subject"`
			Publication string                                 `json:"publication"`
			Dispatch    string                                 `json:"dispatch"`
			Ownership   forgestate.IntegrationDispatchClaim    `json:"ownership"`
		} `json:"prepared"`
		PreparedBy forgestate.IntegrationPreparationSource `json:"preparedBy"`
	}
	if err := json.Unmarshal(plan.Materialization.Payload, &receipt); err != nil {
		return err
	}
	request := receipt.Request
	_, digest, err := forgestate.CanonicalIntegrationPreparation(request)
	if err != nil || digest != plan.RequestDigest ||
		receipt.Schema != 1 || receipt.Protocol != "integration-materializer-receipt-v1" ||
		request.Schema != 1 || request.Protocol != "integration-prefix-v1" || request.Engine != "hive" ||
		request.Repo != plan.Recipe.Repo || request.Base != plan.Recipe.Base ||
		request.BaseSHA != plan.Recipe.BaseSHA || request.Policy != plan.Recipe.Policy ||
		request.Generation != int64(plan.Attempt) || request.CreatedAt != plan.AttemptAt.UTC().Format(time.RFC3339Nano) ||
		!reflect.DeepEqual(request.Members, plan.Recipe.Members) ||
		receipt.PreparedBy.RequestDigest != plan.RequestDigest || receipt.PreparedBy.SourceSHA != plan.Materialization.WorkflowSHA {
		return fmt.Errorf("archived materializer changed exact durable request")
	}
	prepared := []forgestate.IntegrationCandidateSubject{}
	for index, entry := range receipt.Prepared {
		subject := entry.Subject
		if index >= len(request.Members) || !reflect.DeepEqual(subject.Members, request.Members[:index+1]) ||
			subject.Repo != request.Repo || subject.Base != request.Base || subject.BaseSHA != request.BaseSHA ||
			subject.Policy != request.Policy || subject.WorkflowTree != plan.Recipe.WorkflowTree ||
			len(subject.Prefixes) != index+1 || subject.PreparedBy != receipt.PreparedBy {
			return fmt.Errorf("archived materializer changed immutable prefix chain")
		}
		if entry.Publication == "accepted" {
			prepared = append(prepared, subject)
		}
	}
	if len(prepared) != len(plan.Prepared) {
		return fmt.Errorf("archived publication count changed")
	}
	for index := range prepared {
		if !prepared[index].SameCore(plan.Prepared[index]) {
			return fmt.Errorf("archived published subject changed")
		}
	}
	return nil
}
func validateExecutionArchiveBody(proof ExecutionProof) error {
	a := *proof.Archive
	if err := validateArchivedIdentity(a, proof.Subject.Repo, proof.RunID, proof.Attempt); err != nil {
		return err
	}
	var payload struct {
		Schema   int                                    `json:"schema"`
		Protocol string                                 `json:"protocol"`
		Subject  forgestate.IntegrationCandidateSubject `json:"subject"`
		RunID    string                                 `json:"runID"`
		Attempt  string                                 `json:"runAttempt"`
		Outcomes struct{ CI, Tests, Security string }   `json:"outcomes"`
	}
	if err := json.NewDecoder(bytes.NewReader(a.Payload)).Decode(&payload); err != nil {
		return err
	}
	if payload.Schema != 1 || payload.Protocol != "integration-producer-receipt-v1" ||
		!payload.Subject.SameCore(proof.Subject) || payload.RunID != strconv.FormatInt(proof.RunID, 10) ||
		payload.Attempt != strconv.Itoa(proof.Attempt) ||
		proof.State == Passed && (payload.Outcomes.CI != "success" || payload.Outcomes.Tests != "success" || payload.Outcomes.Security != "success") {
		return fmt.Errorf("archived payload changed exact candidate/outcome binding")
	}
	return nil
}
