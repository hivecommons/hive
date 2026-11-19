package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

func TestWorkflowEmitChecksWithoutClient(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme"}}
	emit := workflowEmitChecks(cfg, func() *github.Client { return nil })
	if _, err := emit(context.Background(), "widgets", &workflowmodel.Model{}); !errors.Is(err, errNoForgeClient) {
		t.Fatalf("err = %v", err)
	}
}
