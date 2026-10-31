package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/forgestate"
)

func (c *Client) verifyIntegrationProducerClosure(ctx context.Context, repo, commit, tree string) error {
	parts := strings.Split(repo, "/")
	var response struct {
		Repository struct {
			Name    string `json:"nameWithOwner"`
			Closure struct {
				OID string `json:"oid"`
			} `json:"closure"`
		} `json:"repository"`
	}
	err := c.graphQL(ctx, `query($owner:String!,$name:String!,$closure:String!){
repository(owner:$owner,name:$name){nameWithOwner closure:object(expression:$closure){... on Tree{oid}}}}`,
		map[string]any{"owner": parts[0], "name": parts[1], "closure": commit + ":.github"}, &response)
	if err != nil {
		return err
	}
	if response.Repository.Name != repo || response.Repository.Closure.OID != tree {
		return fmt.Errorf("actual producer source workflow closure differs from enrollment")
	}
	return nil
}

func (c *Client) integrationCandidateArtifact(ctx context.Context, repo string, run *gh.WorkflowRun) (int64, string, error) {
	return c.integrationProducerArtifact(ctx, repo, run, integrationReceiptArtifact)
}

func (c *Client) integrationProducerArtifact(ctx context.Context, repo string, run *gh.WorkflowRun, artifactPrefix string) (int64, string, error) {
	var listing struct {
		TotalCount int `json:"total_count"`
		Artifacts  []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Expired bool   `json:"expired"`
			Size    int64  `json:"size_in_bytes"`
			Digest  string `json:"digest"`
			Run     struct {
				ID   int64  `json:"id"`
				Head string `json:"head_sha"`
			} `json:"workflow_run"`
		} `json:"artifacts"`
	}
	path := fmt.Sprintf("repos/%s/actions/runs/%d/artifacts?per_page=%d", repo, run.GetID(), integrationGroupMaxEntries)
	if err := c.integrationCandidateRead(ctx, path, &listing); err != nil {
		return 0, "", err
	}
	if listing.TotalCount != len(listing.Artifacts) {
		return 0, "", fmt.Errorf("artifact cohort incomplete")
	}
	expectedName := fmt.Sprintf("%s%d", artifactPrefix, run.GetRunAttempt())
	var id int64
	digest := ""
	for _, artifact := range listing.Artifacts {
		if artifact.Name != expectedName {
			continue
		}
		if id != 0 || artifact.ID <= 0 || artifact.Expired || artifact.Size <= 0 ||
			artifact.Size > integrationArtifactMaxBytes || artifact.Run.ID != run.GetID() ||
			artifact.Run.Head != run.GetHeadSHA() {
			return 0, "", fmt.Errorf("producer artifact identity expired, ambiguous or unbound")
		}
		id, digest = artifact.ID, artifact.Digest
	}
	if id == 0 {
		return 0, "", fmt.Errorf("immutable attempt-specific receipt artifact unavailable")
	}
	return id, digest, nil
}

func (c *Client) verifyIntegrationCandidateObjects(ctx context.Context, subject forgestate.IntegrationCandidateSubject) error {
	parts := strings.Split(subject.Repo, "/")
	last := subject.Prefixes[len(subject.Prefixes)-1]
	declarations := []string{"$owner:String!", "$name:String!", "$closure:String!"}
	fields := make([]string, 0, len(subject.Prefixes))
	variables := map[string]any{"owner": parts[0], "name": parts[1], "closure": last.Commit + ":.github"}
	for i, prefix := range subject.Prefixes {
		key := fmt.Sprintf("prefix%d", i)
		declarations = append(declarations, "$"+key+":String!")
		variables[key] = prefix.Commit
		fields = append(fields, key+":object(expression:$"+key+"){... on Commit{oid tree{oid} parents(first:3){nodes{oid}}}}")
	}
	query := "query(" + strings.Join(declarations, ",") + "){repository(owner:$owner,name:$name){nameWithOwner closure:object(expression:$closure){... on Tree{oid}} " + strings.Join(fields, " ") + "}}"
	var response struct {
		Repository map[string]json.RawMessage `json:"repository"`
	}
	if err := c.graphQL(ctx, query, variables, &response); err != nil {
		return err
	}
	var name string
	var closure struct {
		OID string `json:"oid"`
	}
	if err := json.Unmarshal(response.Repository["nameWithOwner"], &name); err != nil {
		return err
	}
	if err := json.Unmarshal(response.Repository["closure"], &closure); err != nil {
		return err
	}
	if name != subject.Repo || closure.OID != subject.WorkflowTree {
		return fmt.Errorf("actual candidate repository or installed workflow closure changed")
	}
	parent := subject.BaseSHA
	for i, expected := range subject.Prefixes {
		var actual struct {
			OID  string `json:"oid"`
			Tree struct {
				OID string `json:"oid"`
			} `json:"tree"`
			Parents struct {
				Nodes []struct {
					OID string `json:"oid"`
				} `json:"nodes"`
			} `json:"parents"`
		}
		if err := json.Unmarshal(response.Repository[fmt.Sprintf("prefix%d", i)], &actual); err != nil {
			return err
		}
		if actual.OID != expected.Commit || actual.Tree.OID != expected.Tree || len(actual.Parents.Nodes) != 2 ||
			actual.Parents.Nodes[0].OID != parent || actual.Parents.Nodes[1].OID != subject.Members[i].Head {
			return fmt.Errorf("actual ordered candidate prefix %d commit/tree/parents changed", i+1)
		}
		parent = expected.Commit
	}
	return nil
}
