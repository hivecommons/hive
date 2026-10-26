package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/hivecap"
)

// sidecarCaps is the hive end of the callback for the proxy seams: a canary
// registry with one planted token, a usage ledger and a REST ledger.
type sidecarCaps struct {
	mu     sync.Mutex
	canary string
	usage  []callback.UsageRecord
	rest   []callback.RESTRecord
}

func (c *sidecarCaps) MintToken(context.Context, callback.MintTokenRequest) (callback.MintTokenResponse, error) {
	return callback.MintTokenResponse{}, callback.ErrNotConfigured
}

func (c *sidecarCaps) RecordREST(_ context.Context, rec callback.RESTRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rest = append(c.rest, rec)
	return nil
}

func (c *sidecarCaps) RateLimit(context.Context) (callback.RateLimitSnapshot, error) {
	return callback.RateLimitSnapshot{Remaining: 4321, Reset: time.Now().Add(time.Hour).Truncate(time.Second), OK: true}, nil
}

func (c *sidecarCaps) RecordUsage(_ context.Context, rec callback.UsageRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usage = append(c.usage, rec)
	return nil
}

func (c *sidecarCaps) ScanCanary(_ context.Context, req callback.CanaryScanRequest) (callback.CanaryScanResponse, error) {
	if c.canary != "" && strings.Contains(req.Text, c.canary) {
		return callback.CanaryScanResponse{Detected: true, Agent: req.Agent, Source: req.Source}, nil
	}
	return callback.CanaryScanResponse{}, nil
}

func (c *sidecarCaps) ScanText(context.Context, callback.TextScanRequest) (callback.TextScanResponse, error) {
	return callback.TextScanResponse{}, nil
}

func (c *sidecarCaps) ReportCondition(context.Context, callback.ConditionReport) error { return nil }

func (c *sidecarCaps) LinearCredential(context.Context) (callback.LinearCredential, error) {
	return callback.LinearCredential{}, callback.ErrNotConfigured
}

func (c *sidecarCaps) CreatePR(context.Context, callback.CreatePRRequest) (callback.CreatePRResponse, error) {
	return callback.CreatePRResponse{}, callback.ErrNotConfigured
}

// serveCallback starts hive's end on sock and returns its stop function.
func serveCallback(t *testing.T, sock string, caps callback.Capabilities) func() {
	t.Helper()
	ls, err := callback.Listen(callback.ListenConfig{Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- callback.NewServer(caps, "").Serve(ctx, ls) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}
}

// A proxy whose Accounting / Usage / Canary seams are the hive callback
// adapters (the sidecar arrangement, hivecommons/hive#14138 stage 4)
// refuses GitHub writes while hive is unreachable, reports no shared
// rate-limit snapshot and records no usage; when hive comes back the same
// proxy, with nothing re-installed, lets a clean write through, blocks a
// canary leak, and its usage and REST records reach hive.
func TestSidecarProxySeamsRefuseWhileHiveIsAwayAndResume(t *testing.T) {
	dir, err := os.MkdirTemp("", "sidecar-proxy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, callback.SocketName)
	client, err := callback.NewClient("unix://"+sock, "")
	if err != nil {
		t.Fatal(err)
	}
	set := hivecap.New(client, 300*time.Millisecond)

	var leaks []crux.CanaryLeak
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	p.SetGitHubRESTAccounting(set.Accounting)
	p.SetTokenSink(set.Usage)
	p.SetCanaryScanner(true, true, set.Canary, func(l crux.CanaryLeak) { leaks = append(leaks, l) })

	write := func(body string) *http.Request {
		req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues/1/comments", strings.NewReader(body))
		return req
	}

	// Hive away: the write is refused, the refusal says why, nothing is
	// recorded anywhere, and the budget sees no shared snapshot.
	reason, deny, detected := p.inspectCanaryEgress("worker", write(`{"body":"hello"}`))
	if !deny || !detected || reason == "" {
		t.Fatalf("hive away: deny=%v detected=%v reason=%q, want refused", deny, detected, reason)
	}
	if len(leaks) != 1 || !strings.HasSuffix(leaks[0].Source, hivecap.UnavailableSourceSuffix) || leaks[0].Token != "" {
		t.Fatalf("hive away: leak record = %+v, want unavailable-suffixed source and no token", leaks)
	}
	if _, _, ok := p.restAccounting.CoreRateLimitSnapshot(); ok {
		t.Fatal("hive away: snapshot must be ok=false")
	}
	p.tokenSink.Record("worker", "m", 1, 2)
	p.restAccounting.RecordRESTRequest("worker", http.MethodGet, "/x", http.StatusOK, nil)

	// Hive back, same proxy: a clean write passes, the planted canary is
	// blocked as a real detection, and the ledgers fill.
	caps := &sidecarCaps{canary: "CANARY-worker-7"}
	stop := serveCallback(t, sock, caps)
	defer stop()
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("callback never ready: %v", err)
	}
	leaks = nil
	if reason, deny, detected := p.inspectCanaryEgress("worker", write(`{"body":"hello"}`)); deny || detected || reason != "" {
		t.Fatalf("hive back: clean write deny=%v detected=%v reason=%q", deny, detected, reason)
	}
	if reason, deny, detected := p.inspectCanaryEgress("worker", write(`{"body":"CANARY-worker-7"}`)); !deny || !detected || reason == "" {
		t.Fatalf("hive back: leak deny=%v detected=%v reason=%q", deny, detected, reason)
	}
	if len(leaks) != 1 || leaks[0].Agent != "worker" || strings.HasSuffix(leaks[0].Source, hivecap.UnavailableSourceSuffix) {
		t.Fatalf("hive back: leak record = %+v", leaks)
	}
	remaining, reset, ok := p.restAccounting.CoreRateLimitSnapshot()
	if !ok || remaining != 4321 || reset.IsZero() {
		t.Fatalf("hive back: snapshot = %d %v %v", remaining, reset, ok)
	}
	p.tokenSink.Record("worker", "m", 10, 20)
	p.tokenSink.RecordCopilot("worker", "m", 1, 1)
	p.restAccounting.RecordInjectedRESTRequest("worker", http.MethodGet, "/y", http.StatusOK, http.Header{"X-Ratelimit-Remaining": {"9"}})
	caps.mu.Lock()
	defer caps.mu.Unlock()
	if len(caps.usage) != 2 || caps.usage[0].InputTokens != 10 || caps.usage[1].Kind != callback.UsageKindCopilot {
		t.Fatalf("usage at hive = %+v, want only the records made while reachable", caps.usage)
	}
	if len(caps.rest) != 1 || !caps.rest[0].Injected || caps.rest[0].Path != "/y" {
		t.Fatalf("rest at hive = %+v", caps.rest)
	}
}
