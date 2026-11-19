package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestWorkflowRepoFilesWithoutClient(t *testing.T) {
	fetch := workflowRepoFiles(func() *github.Client { return nil })
	if _, _, err := fetch(context.Background(), "acme/widgets"); !errors.Is(err, errNoForgeClient) {
		t.Fatalf("err = %v", err)
	}
}
