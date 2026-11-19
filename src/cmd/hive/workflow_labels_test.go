package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestWorkflowProvisionLabelsWithoutClient(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme"}}
	provision := workflowProvisionLabels(cfg, func() *github.Client { return nil }, nil)
	if err := provision(context.Background(), "widgets"); !errors.Is(err, errNoForgeClient) {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkflowProvisionLabelsRejectsBareRepoWithoutOrg(t *testing.T) {
	cfg := &config.Config{}
	provision := workflowProvisionLabels(cfg, func() *github.Client { return &github.Client{} }, nil)
	err := provision(context.Background(), "widgets")
	if err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("err = %v", err)
	}
}
