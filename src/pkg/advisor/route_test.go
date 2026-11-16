package advisor

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// On the Claude CLI route the review is one CLI turn: the system prompt and
// the turn go to the CLI with the model and effort, the endpoint is never
// called, usage is estimated for the budget, and the record names the route
// with no per-token cost.
func TestServerReviewsThroughTheClaudeCLI(t *testing.T) {
	type call struct{ system, user, model, effort string }
	var calls []call
	store := NewStore("")
	srv := newTestServer(store, &testUsage{}, "http://127.0.0.1:1", Runtime{Route: RouteClaudeCLI, Model: "claude-haiku-5-5", ReasoningEffort: "high"})
	srv.CLI = func(ctx context.Context, system, user, model, effort string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the CLI turn has no deadline")
		}
		calls = append(calls, call{system, user, model, effort})
		return `{"severity":"concern","text":"check the base branch"}`, nil
	}

	rec, resp := postAdvise(t, srv.Handler(), "scout", `{"turn":"t1","transcript":"pushed to main"}`)
	if rec.Code != http.StatusOK || resp.Severity != SeverityConcern {
		t.Fatalf("status = %d response = %+v", rec.Code, resp)
	}
	if len(calls) != 1 || calls[0].model != "claude-haiku-5-5" || calls[0].effort != "high" || calls[0].system == "" || calls[0].user == "" {
		t.Fatalf("CLI calls = %+v", calls)
	}
	records := store.List("scout", time.Time{}, 0)
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	r := records[0]
	if r.Route != RouteClaudeCLI || r.CostUSD != 0 || !r.UsageEstimated || r.InputTokens == 0 || r.Text != "check the base branch" {
		t.Fatalf("record = %+v", r)
	}
}

// A route that is not ready (no endpoint, or Claude not signed in) skips the
// review as unreachable without calling anything; a CLI route with no CLI
// wired does the same; an HTTP review records its route.
func TestServerRouteSkipsAndHTTPRoute(t *testing.T) {
	store := NewStore("")
	cliCalled := false
	cli := func(context.Context, string, string, string, string) (string, error) {
		cliCalled = true
		return "", errors.New("must not run")
	}

	unready := newTestServer(store, &testUsage{}, "http://127.0.0.1:1", Runtime{Route: RouteUnready, UnreadyReason: "Claude is not signed in"})
	unready.CLI = cli
	postAdvise(t, unready.Handler(), "scout", `{"transcript":"a"}`)

	unwired := newTestServer(store, &testUsage{}, "http://127.0.0.1:1", Runtime{Route: RouteClaudeCLI})
	postAdvise(t, unwired.Handler(), "scout", `{"transcript":"b"}`)

	chat := fakeChatServer(t, http.StatusOK, `{"severity":"aside","text":""}`, nil)
	defer chat.Close()
	viaHTTP := newTestServer(store, &testUsage{}, chat.URL, Runtime{})
	postAdvise(t, viaHTTP.Handler(), "scout", `{"transcript":"c"}`)

	if cliCalled {
		t.Fatal("a skipped review ran the CLI")
	}
	records := store.List("scout", time.Time{}, 0)
	if len(records) != 3 {
		t.Fatalf("records = %+v", records)
	}
	byRoute := map[string]Record{}
	for _, r := range records {
		byRoute[r.Route] = r
	}
	if r := byRoute[RouteUnready]; r.Skipped != SkipUnreachable {
		t.Errorf("unready record = %+v", r)
	}
	if r := byRoute[RouteClaudeCLI]; r.Skipped != SkipUnreachable {
		t.Errorf("unwired CLI record = %+v", r)
	}
	if r := byRoute[RouteHTTP]; r.Skipped != "" || r.Severity != SeverityAside {
		t.Errorf("http record = %+v", r)
	}
}

// The CLI turn's deadline is the review timeout: a CLI that runs past it is
// recorded as a timeout skip.
func TestEvaluatorCLITimeout(t *testing.T) {
	e, err := NewEvaluator(Config{Route: RouteClaudeCLI, TimeoutS: 1, CLI: func(ctx context.Context, _, _, _, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Review(context.Background(), "scout", "turn"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
}
