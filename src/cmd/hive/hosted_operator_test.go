package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHostedOperationRequestStrictEnvelopeAndFreshness(t *testing.T) {
	config := hostedOperatorLedgerConfig()
	now := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)
	request := hostedOperatorLedgerRequest(t, config, now)
	identity := installedReleaseIdentity{
		Repository: integratedReleaseRepository, Version: config.HiveReleaseVersion, HiveCommit: config.HiveCommit,
		VisualHiveCommit: config.VisualHiveRef, ManifestSHA256: config.DistributionManifestSHA256,
		HostedControllerProtocol: config.HostedControllerProtocol,
	}
	t.Setenv("GITHUB_SHA", request.DefaultHeadSHA)
	encoded, digest, exact, err := encodeHostedOperationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, decodedBytes, err := decodeHostedOperationRequest(encoded, digest)
	if err != nil || !strings.EqualFold(decoded.RequestID, request.RequestID) || string(decodedBytes) != string(exact) {
		t.Fatalf("exact request round trip failed: decoded=%+v err=%v", decoded, err)
	}
	if err := validateHostedOperationRequestEnvelope(decoded, config, identity, request.Operation, request.RequestID); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	wrongProtocol := decoded
	wrongProtocol.Release.HostedControllerProtocol = 0
	if err := validateHostedOperationRequestEnvelope(wrongProtocol, config, identity, request.Operation, request.RequestID); err == nil {
		t.Fatal("operator request without the exact hosted controller protocol was accepted")
	}
	if err := validateHostedOperationRequestFresh(decoded, request.ExpectedStateHeadSHA, request.DefaultHeadSHA, now); err != nil {
		t.Fatalf("fresh request rejected: %v", err)
	}
	if err := validateHostedOperationRequestFresh(decoded, strings.Repeat("9", 40), request.DefaultHeadSHA, now); err == nil {
		t.Fatal("stale state head was accepted")
	}
	if err := validateHostedOperationRequestFresh(decoded, request.ExpectedStateHeadSHA, strings.Repeat("8", 40), now); err == nil {
		t.Fatal("stale default head was accepted on first use")
	}
	if err := validateHostedOperationRequestFresh(decoded, request.ExpectedStateHeadSHA, request.DefaultHeadSHA, request.ExpiresAt.Add(time.Second)); err == nil {
		t.Fatal("expired request was accepted on first use")
	}

	if _, _, err := decodeHostedOperationRequest(encoded, strings.Repeat("0", 64)); err == nil {
		t.Fatal("request digest mismatch was accepted")
	}
	withTrailing := append(append([]byte(nil), exact...), []byte("{}")...)
	trailingDigest := sha256.Sum256(withTrailing)
	if _, _, err := decodeHostedOperationRequest(base64.RawURLEncoding.EncodeToString(withTrailing), hex.EncodeToString(trailingDigest[:])); err == nil {
		t.Fatal("request with a second JSON document was accepted")
	}

	var unknown map[string]any
	if err := json.Unmarshal(exact, &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["unexpected_authority"] = true
	unknownBytes, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	unknownDigest := sha256.Sum256(unknownBytes)
	if _, _, err := decodeHostedOperationRequest(base64.RawURLEncoding.EncodeToString(unknownBytes), hex.EncodeToString(unknownDigest[:])); err == nil {
		t.Fatal("request with an unknown authority field was accepted")
	}
}
