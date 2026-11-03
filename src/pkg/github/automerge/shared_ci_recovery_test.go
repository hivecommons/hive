package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

type recoveryBlockTransport struct {
	Transport
	blocked bool
}

func (t *recoveryBlockTransport) SharedCIRerunBlocked(repo string, number int, head string) (hgithub.SharedCIRerunBlock, bool) {
	return hgithub.SharedCIRerunBlock{HeadSHA: head, RunID: 11}, t.blocked
}

func TestSharedCIRecoveryUsesUnauthorizedHandoffAndClearsOnRecovery(t *testing.T) {
	api := &stageAPI{statefulComments: true}
	srv := newStageAPI(t, api)
	defer srv.Close()
	e, _, _ := newStageEngine(t, srv.URL, &contributorLaneClock{at: time.Now()})
	transport := &recoveryBlockTransport{Transport: e.transport, blocked: true}
	e.transport = transport
	obs := stageObservation{Owner: "acme", Repo: "widget", Display: "acme/widget", Number: 42, HeadSHA: "aaa"}
	ctx := context.Background()
	if step := e.sharedCIRecoveryUnauthorized(ctx, obs); step != unauthorizedStepRerunSharedCI {
		t.Fatalf("step = %q", step)
	}
	e.sharedCIRecoveryUnauthorized(ctx, obs)
	if len(api.comments) != 1 || !strings.Contains(api.comments[0], "actions: write") ||
		!strings.Contains(api.comments[0], "/actions/runs/11") ||
		!strings.Contains(api.comments[0], "not a source-code failure") {
		t.Fatalf("handoff must be actionable and head-deduplicated: %v", api.comments)
	}
	transport.blocked = false
	obs.Labels = []string{unauthorizedLabel}
	obs.HeadSHA = "new-head"
	if step := e.sharedCIRecoveryUnauthorized(ctx, obs); step != "" {
		t.Fatalf("recovered step = %q", step)
	}
	if len(api.removed) != 1 || api.removed[0] != unauthorizedLabel {
		t.Fatalf("recovery/head replacement did not clear its own label: %v", api.removed)
	}
}
