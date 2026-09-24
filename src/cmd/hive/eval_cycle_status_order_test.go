package main

import (
	"os"
	"strings"
	"testing"
)

func TestEvalCyclePublishesStatusBeforeDispatchingKicks(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	publish := strings.Index(body, "if !statusPublished {\n\t\tdashSrv.UpdateStatusIfFresh(statusPayload, buildEpoch)\n\t}")
	if publish < 0 {
		t.Fatal("final status publish block not found")
	}
	dispatch := strings.Index(body, "dispatchAgentKicks(messages, releaseProviderBudgetProbe")
	if dispatch < 0 {
		t.Fatal("kick dispatch block not found")
	}
	if dispatch < publish {
		t.Fatalf("kick dispatch occurs before final status publish; /api/status and S8 can be starved by blocking SendKick")
	}
}
