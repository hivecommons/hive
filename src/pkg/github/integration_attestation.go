package github

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

const (
	integrationSubjectArtifactPrefix = "hive-integration-subject-v1-"
	integrationSubjectFile           = "subject.json"
	integrationArtifactMaxBytes      = 2 << 20
	integrationSubjectMaxBytes       = 64 << 10
	integrationArtifactTimeout       = 15 * time.Second
	integrationOIDBytes              = 20
)

func integrationOID(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == integrationOIDBytes
}

// DirectIntegrationAttestation is supplied by the pinned workflow itself.
// A run's head SHA alone does not establish its actual checkout or tested tree.
type DirectIntegrationAttestation struct {
	Version    int                       `json:"version"`
	Repo       string                    `json:"repo"`
	Base       string                    `json:"base"`
	BaseSHA    string                    `json:"base_sha"`
	TestedSHA  string                    `json:"tested_sha"`
	TestedTree string                    `json:"tested_tree"`
	RunID      int64                     `json:"run_id"`
	Attempt    int                       `json:"attempt"`
	Policy     string                    `json:"policy"`
	Members    []DirectIntegrationMember `json:"members"`
}

type DirectIntegrationMember struct {
	PR   int    `json:"pr"`
	Head string `json:"head"`
}

// ReadDirectIntegrationAttestation never produces a candidate or executes Git.
// The external pinned producer must publish the actual checkout identity.
func (c *Client) ReadDirectIntegrationAttestation(ctx context.Context, policy config.IntegrationPolicy, run *gh.WorkflowRun) (DirectIntegrationAttestation, error) {
	var out DirectIntegrationAttestation
	if run == nil || run.GetEvent() != "pull_request" || run.GetID() <= 0 || run.GetRunAttempt() < 1 || len(run.PullRequests) != 1 ||
		run.PullRequests[0] == nil || run.GetStatus() != "completed" || policy.Engine != "direct" {
		return out, fmt.Errorf("direct tested-subject producer unavailable")
	}
	request, err := c.client.NewRequest("GET", fmt.Sprintf("repos/%s/actions/runs/%d/artifacts?per_page=%d", policy.Repo, run.GetID(), integrationGroupMaxEntries), nil)
	if err != nil {
		return out, err
	}
	var listing struct {
		Artifacts []struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Expired     bool   `json:"expired"`
			Size        int64  `json:"size_in_bytes"`
			WorkflowRun struct {
				ID      int64  `json:"id"`
				HeadSHA string `json:"head_sha"`
			} `json:"workflow_run"`
		} `json:"artifacts"`
	}
	response, err := c.client.Do(ctx, request, &listing)
	if err != nil {
		return out, err
	}
	if response != nil && response.NextPage != 0 {
		return out, fmt.Errorf("direct attestation artifact cohort truncated")
	}
	var artifactID int64
	name := fmt.Sprintf("%s%d", integrationSubjectArtifactPrefix, run.GetRunAttempt())
	for _, artifact := range listing.Artifacts {
		if artifact.Name != name {
			continue
		}
		if artifactID != 0 || artifact.Expired || artifact.ID <= 0 || artifact.Size > integrationArtifactMaxBytes ||
			artifact.WorkflowRun.ID != run.GetID() || artifact.WorkflowRun.HeadSHA != run.GetHeadSHA() {
			return out, fmt.Errorf("direct attestation artifact expired, ambiguous or unbound")
		}
		artifactID = artifact.ID
	}
	if artifactID == 0 {
		return out, fmt.Errorf("pinned workflow did not publish %s; actual tested subject remains unknown", name)
	}
	parts := strings.SplitN(policy.Repo, "/", 2)
	if len(parts) != 2 {
		return out, fmt.Errorf("invalid direct repository")
	}
	location, _, err := c.client.Actions.DownloadArtifact(ctx, parts[0], parts[1], artifactID, 0)
	if err != nil {
		return out, err
	}
	if location == nil || location.Scheme != "https" && !(c.client.BaseURL.Scheme == "http" && location.Scheme == "http" && location.Host == c.client.BaseURL.Host) {
		return out, fmt.Errorf("direct attestation download requires HTTPS")
	}
	download, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return out, err
	}
	// Deliberately do not use the authenticated GitHub transport for an
	// expiring artifact-storage URL.
	client := &http.Client{Timeout: integrationArtifactTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	payload, err := client.Do(download)
	if err != nil {
		return out, err
	}
	defer payload.Body.Close()
	if payload.StatusCode != http.StatusOK {
		return out, fmt.Errorf("direct artifact download HTTP %d", payload.StatusCode)
	}
	compressed, err := io.ReadAll(io.LimitReader(payload.Body, integrationArtifactMaxBytes+1))
	if err != nil {
		return out, err
	}
	if len(compressed) > integrationArtifactMaxBytes {
		return out, fmt.Errorf("direct artifact exceeds compressed bound")
	}
	out, err = decodeDirectIntegrationAttestation(compressed)
	if err != nil {
		return out, err
	}
	pr := run.PullRequests[0]
	if out.Version != 1 || !strings.EqualFold(out.Repo, policy.Repo) || out.Base != policy.Base || out.Base != pr.GetBase().GetRef() ||
		out.BaseSHA != pr.GetBase().GetSHA() || out.RunID != run.GetID() || out.Attempt != run.GetRunAttempt() || out.Policy != policy.Fingerprint() ||
		!integrationOID(out.BaseSHA) || !integrationOID(out.TestedSHA) || !integrationOID(out.TestedTree) ||
		len(out.Members) != 1 || out.Members[0].PR != pr.GetNumber() || out.Members[0].Head != pr.GetHead().GetSHA() || !integrationOID(out.Members[0].Head) {
		return DirectIntegrationAttestation{}, fmt.Errorf("direct subject is not bound to exact policy, workflow attempt and pull-request event")
	}
	commit, _, err := c.client.Git.GetCommit(ctx, parts[0], parts[1], out.TestedSHA)
	if err != nil {
		return DirectIntegrationAttestation{}, err
	}
	if commit == nil || commit.GetSHA() != out.TestedSHA || commit.GetTree().GetSHA() != out.TestedTree || len(commit.Parents) != 2 {
		return DirectIntegrationAttestation{}, fmt.Errorf("direct tested commit/tree not verified")
	}
	a, b := commit.Parents[0].GetSHA(), commit.Parents[1].GetSHA()
	if out.BaseSHA == out.Members[0].Head || !(a == out.BaseSHA && b == out.Members[0].Head || b == out.BaseSHA && a == out.Members[0].Head) {
		return DirectIntegrationAttestation{}, fmt.Errorf("direct tested merge parents do not bind current event base and author head")
	}
	return out, nil
}

func decodeDirectIntegrationAttestation(compressed []byte) (DirectIntegrationAttestation, error) {
	var out DirectIntegrationAttestation
	archive, err := zip.NewReader(bytes.NewReader(compressed), int64(len(compressed)))
	if err != nil {
		return out, err
	}
	if len(archive.File) != 1 || archive.File[0].Name != integrationSubjectFile || archive.File[0].UncompressedSize64 > integrationSubjectMaxBytes {
		return out, fmt.Errorf("direct attestation must contain one bounded subject.json")
	}
	file, err := archive.File[0].Open()
	if err != nil {
		return out, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, integrationSubjectMaxBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return out, fmt.Errorf("direct attestation has trailing or oversized content")
	}
	return out, nil
}
