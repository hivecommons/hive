package advisoryengine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hivetool"
)

func runOne(t *testing.T, cfg *config.Config) Result {
	t.Helper()
	rec := &recorder{}
	e := newEngine(&reader{}, rec, public)
	if n := e.Observe(context.Background(), cfg, items); n != 1 {
		t.Fatalf("Observe started %d", n)
	}
	e.Wait()
	if len(rec.got) != 1 {
		t.Fatalf("records = %d", len(rec.got))
	}
	return rec.got[0]
}

// TestNoUsableGateway: empty, unknown and invalid gateway references are
// dropped, and an item with none left is recorded as skipped.
func TestNoUsableGateway(t *testing.T) {
	cfg := cfgWith(true, config.GatewayConfig{Name: "bad", Endpoint: "ftp://x", DefaultModel: "m"})
	cfg.AdvisoryEngine.Gateways = append(cfg.AdvisoryEngine.Gateways,
		config.AdvisoryEngineGateway{Name: ""}, config.AdvisoryEngineGateway{Name: "missing"})
	if r := runOne(t, cfg); !strings.Contains(r.Skipped, "no usable") {
		t.Fatalf("result = %+v", r)
	}
}

// TestProseFinishIsGated: prose before the finish phase moves the session
// to it, and prose in the finish phase is accepted only through the
// evidence gate.
func TestProseFinishIsGated(t *testing.T) {
	g := &gateway{script: [][2]string{
		{"repo__read_file", `{"path":"src/a.go"}`},
		{"", "I think it is src/a.go:3."},
		{"", "Root cause: Rebase is empty at src/a.go:3."},
	}}
	r := runOne(t, cfgWith(true, gw("gw", g.serve(t).URL, false)))
	if !r.Implicit || r.Outcome != hivetool.ToolTaskComplete || r.Verified != 1 {
		t.Fatalf("result = %+v", r)
	}
}

// TestRejectedRequestRecordsError: a provider rejection ends the session
// with the error recorded, not retried elsewhere.
func TestRejectedRequestRecordsError(t *testing.T) {
	g := &gateway{script: [][2]string{{"!400", ""}}}
	cfg := cfgWith(true, gw("gw", g.serve(t).URL, false))
	cfg.AdvisoryEngine.MaxTurns = 3
	if r := runOne(t, cfg); r.Error == "" || r.Outcome != "" {
		t.Fatalf("result = %+v", r)
	}
}

// TestObserveBusyAndBounds: one batch at a time, invalid items ignored, a
// nil reader or engine does nothing, and the seen set is bounded.
func TestObserveBusyAndBounds(t *testing.T) {
	g := &gateway{script: triageScript(), block: make(chan struct{})}
	cfg := cfgWith(true, gw("gw", g.serve(t).URL, false))
	e := newEngine(&reader{}, &recorder{}, public)
	for i := 0; i < maxSeen; i++ {
		e.seen[strings.Repeat("k", i+1)] = true
	}
	batch := append([]Item{{Repo: "", Number: 1}, {Repo: "o/r", Number: 0}}, items...)
	if n := e.Observe(context.Background(), cfg, batch); n != 1 {
		t.Fatalf("first Observe = %d", n)
	}
	if len(e.seen) != 1 {
		t.Fatalf("seen set not reset: %d", len(e.seen))
	}
	if n := e.Observe(context.Background(), cfg, []Item{{Repo: "o/r", Number: 9}}); n != 0 {
		t.Fatalf("Observe while running = %d", n)
	}
	close(g.block)
	e.Wait()
	if n := (*Engine)(nil).Observe(context.Background(), cfg, items); n != 0 {
		t.Fatal("nil engine started work")
	}
	if n := New(Deps{}).Observe(context.Background(), cfg, items); n != 0 {
		t.Fatal("engine without a reader started work")
	}
}

func TestWithheldAndClip(t *testing.T) {
	if withheldReason("summary with CANARY-1 inside", "CANARY-1") == "" {
		t.Fatal("canary echo not withheld")
	}
	if got := withheldReason("Rebase is empty at src/a.go:3", "CANARY-1"); got != "" {
		t.Fatalf("clean summary withheld: %s", got)
	}
	if got := clip("abcdef", 3); got != "abc…" {
		t.Fatalf("clip = %q", got)
	}
	if _, ok := ReadOnlyHTTPClient(nil).Transport.(readOnlyTransport); !ok {
		t.Fatal("nil base not wrapped")
	}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/x", nil)
	if _, err := ReadOnlyHTTPClient(nil).Do(req); err == nil {
		t.Fatal("POST allowed")
	}
}

// TestPrivateRecordIsMetadataOnly: for a private or unknown-visibility
// repository neither the recorded result nor the log line carries the
// advice text; the metadata is all there. A public repository keeps it.
func TestPrivateRecordIsMetadataOnly(t *testing.T) {
	unknown := func(context.Context, string) (bool, error) { return false, errors.New("boom") }
	for name, vis := range map[string]func(context.Context, string) (bool, error){"private": private, "unknown": unknown, "public": public} {
		g := &gateway{script: triageScript()}
		var logs bytes.Buffer
		rec := &recorder{}
		e := New(Deps{Reader: &reader{}, RepoPrivate: vis, Record: rec.record, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
		e.Observe(context.Background(), cfgWith(true, gw("gw", g.serve(t).URL, false)), items)
		e.Wait()
		r := rec.got[0]
		if r.Outcome != hivetool.ToolTaskComplete || r.Verified != 1 || r.Turns != 2 || r.Provider != "gw" || r.Model != "m" || r.Requests != 2 || r.Elapsed <= 0 {
			t.Fatalf("%s: metadata missing: %+v", name, r)
		}
		if strings.Contains(logs.String(), "Rebase is a no-op") {
			t.Fatalf("%s: advice text in the log line: %s", name, logs.String())
		}
		if name == "public" {
			if !strings.Contains(r.Summary, "src/a.go:3") {
				t.Fatalf("public summary dropped: %+v", r)
			}
		} else if r.Summary != "" || !r.Private {
			t.Fatalf("%s: advice text recorded: %+v", name, r)
		}
	}
}
