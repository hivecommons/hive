package main

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestAdoptGitHubClientRetiresHistoricalAccountingWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previous, next := &github.Client{}, &github.Client{}
	done := previous.StartPRTerminalEnrichment(ctx)
	b := &boot{ghClient: previous}
	b.adoptGitHubClient(next, nil)
	const stopTimeout = 5 * time.Second
	select {
	case <-done:
	case <-time.After(stopTimeout):
		t.Fatal("client rebuild left old historical accounting worker running")
	}
	if ctx.Err() != nil || b.currentGitHubClient() != next {
		t.Fatal("retiring historical accounting disturbed boot context/client adoption")
	}
}
