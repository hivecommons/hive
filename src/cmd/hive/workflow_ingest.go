package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest/cluster"
)

// errNoForgeClient is returned by the workflow ingest fetcher before a
// GitHub client exists.
var errNoForgeClient = errors.New("no GitHub client")

// workflowRepoFiles is the dashboard's WorkflowRepoFiles dependency
// (POST /api/workflows/ingest, RFC #17620 P3): it downloads the repo's
// default branch read-only, keeps only the CI definition and process
// convention files and removes the checkout. The default branch's rules
// (protection or rulesets, P5) ride along at ingest.BranchRulesPath when
// the forge reports any; a failure to read them is logged, not fatal.
// client is read per call so a credential rebuild is followed.
func workflowRepoFiles(client func() *github.Client) func(ctx context.Context, repo string) (map[string][]byte, string, error) {
	return func(ctx context.Context, repo string) (map[string][]byte, string, error) {
		c := client()
		if c == nil {
			return nil, "", errNoForgeClient
		}
		co, err := c.FetchDefaultBranchTree(ctx, repo)
		if err != nil {
			return nil, "", err
		}
		defer co.Cleanup()
		files, err := ingest.ReadTree(co.Dir)
		if err != nil {
			return nil, "", err
		}
		switch rules, err := c.FetchBranchRules(ctx, repo, co.Ref); {
		case err != nil:
			log.Printf("workflow ingest: %s branch rules: %v", repo, err)
		case rules != nil:
			if b, err := json.Marshal(rules); err == nil {
				files[ingest.BranchRulesPath] = b
			}
		}
		return files, co.Ref, nil
	}
}

// workflowRepoFile is the dashboard's WorkflowRepoFile dependency
// (POST /api/workflows/ingest/spec {repo, path}, RFC #17620 phase 6): one
// default-branch file through the forge API.
func workflowRepoFile(client func() *github.Client) func(ctx context.Context, repo, path string) (string, error) {
	return func(ctx context.Context, repo, path string) (string, error) {
		c := client()
		if c == nil {
			return "", errNoForgeClient
		}
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" {
			return "", errors.New("repo must be owner/name")
		}
		return c.GetFileContent(ctx, owner, name, strings.TrimPrefix(path, "/"))
	}
}

// workflowClusterRunner is the dashboard's WorkflowClusterRunner dependency:
// kubectl on PATH with a kubeconfig or in-cluster token, else nil so the
// cluster ingest route answers 422 instead of failing per call.
func workflowClusterRunner() cluster.Runner {
	if ok, _ := cluster.Available(); !ok {
		return nil
	}
	return cluster.Kubectl{}
}
