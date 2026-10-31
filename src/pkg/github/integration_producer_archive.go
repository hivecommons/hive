package github

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	integrationReceiptFile          = "receipt.json"
	integrationReceiptArtifact      = "integration-producer-receipt-"
	integrationReceiptEnvelopeLimit = 384 << 10
)

type integrationReceiptEnvelope struct {
	Schema  int    `json:"schema"`
	Payload string `json:"payload"`
	Token   string `json:"token"`
}

func decodeIntegrationReceipt(archive []byte) ([]byte, string, error) {
	return decodeIntegrationEnvelope(archive, integrationReceiptFile)
}

func decodeIntegrationEnvelope(archive []byte, filename string) ([]byte, string, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, "", err
	}
	if len(reader.File) != 1 || reader.File[0].Name != filename ||
		reader.File[0].UncompressedSize64 > integrationReceiptEnvelopeLimit {
		return nil, "", fmt.Errorf("producer artifact requires one bounded receipt.json")
	}
	file, err := reader.File[0].Open()
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	var envelope integrationReceiptEnvelope
	decoder := json.NewDecoder(io.LimitReader(file, integrationReceiptEnvelopeLimit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, "", fmt.Errorf("producer envelope has trailing content")
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil || envelope.Schema != 1 || len(envelope.Token) == 0 ||
		len(envelope.Token) > integrationProducerMaxToken || len(payload) == 0 ||
		len(payload) > integrationProducerMaxPayload {
		return nil, "", fmt.Errorf("producer envelope identity or payload bounds rejected")
	}
	return payload, envelope.Token, nil
}

func (c *Client) downloadIntegrationReceipt(ctx context.Context, repo string, artifactID int64, digest string) ([]byte, error) {
	if !strings.HasPrefix(digest, "sha256:") || !integrationProducerDigest(strings.TrimPrefix(digest, "sha256:")) {
		return nil, fmt.Errorf("immutable artifact digest unavailable; completeness unknown")
	}
	parts := strings.Split(repo, "/")
	location, _, err := c.client.Actions.DownloadArtifact(ctx, parts[0], parts[1], artifactID, 0)
	if err != nil {
		return nil, err
	}
	if location == nil || location.User != nil ||
		location.Scheme != "https" && !(c.client.BaseURL.Scheme == "http" &&
			location.Scheme == "http" && location.Host == c.client.BaseURL.Host) {
		return nil, fmt.Errorf("producer artifact download requires HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return nil, err
	}
	// Storage URLs never receive the authenticated forge/App transport.
	client := &http.Client{Timeout: integrationArtifactTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("producer artifact download HTTP %d", response.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, integrationArtifactMaxBytes+1))
	if err != nil || len(archive) > integrationArtifactMaxBytes {
		return nil, fmt.Errorf("producer artifact archive unavailable or oversized")
	}
	actual := sha256.Sum256(archive)
	if "sha256:"+hex.EncodeToString(actual[:]) != digest {
		return nil, fmt.Errorf("producer artifact immutable digest mismatch")
	}
	return archive, nil
}
