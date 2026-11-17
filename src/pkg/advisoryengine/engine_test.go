package advisoryengine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hivetool"
)

// gateway is a scripted OpenAI-compatible endpoint. Each request is answered
// with the next tool call in script; it records the tool names it was offered.
type gateway struct {
	mu      sync.Mutex
	calls   int
	offered map[string]bool
	script  [][2]string // wire tool name, JSON arguments
}

func (g *gateway) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.offered == nil {
			g.offered = map[string]bool{}
		}
		for _, tl := range req.Tools {
			g.offered[strings.ReplaceAll(tl.Function.Name, "__", ".")] = true
		}
		step := g.script[min(g.calls, len(g.script)-1)]
		g.calls++
		args, _ := json.Marshal(step[1])
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"c`+
			string(rune('0'+g.calls))+`","function":{"name":"`+step[0]+`","arguments":`+string(args)+`}}]}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *gateway) n() int { g.mu.Lock(); defer g.mu.Unlock(); return g.calls }

func (g *gateway) offeredNames() map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]bool{}
	for k := range g.offered {
		out[k] = true
	}
	return out
}

func triageScript() [][2]string {
	return [][2]string{
		{"repo__read_file", `{"path":"src/a.go"}`},
		{"task__complete", `{"summary":"Rebase is a no-op at src/a.go:3; implement it."}`},
	}
}

// reader is a hivetool.Reader fake that counts reads.
type reader struct {
	mu    sync.Mutex
	reads int
}

func (r *reader) Issue(context.Context, string, int) (hivetool.Issue, error) {
	return hivetool.Issue{}, errors.New("not found")
}
func (r *reader) List(context.Context, string, string, string) ([]hivetool.Entry, error) {
	return nil, nil
}
func (r *reader) File(_ context.Context, _, _, file string) ([]byte, error) {
	r.mu.Lock()
	r.reads++
	r.mu.Unlock()
	if file != "src/a.go" {
		return nil, errors.New("not found")
	}
	return []byte("package a\n\nfunc Rebase() {}\n"), nil
}
func (r *reader) Checks(context.Context, string, string) ([]hivetool.CheckRun, error) {
	return nil, nil
}

type recorder struct {
	mu  sync.Mutex
	got []Result
}

func (r *recorder) record(x Result) { r.mu.Lock(); r.got = append(r.got, x); r.mu.Unlock() }

func cfgWith(enabled bool, gws ...config.GatewayConfig) *config.Config {
	c := &config.Config{}
	c.Governor.Gateways = gws
	c.AdvisoryEngine.Enabled = enabled
	for _, g := range gws {
		c.AdvisoryEngine.Gateways = append(c.AdvisoryEngine.Gateways, config.AdvisoryEngineGateway{Name: g.Name})
	}
	return c
}

func gw(name, url string, trains bool) config.GatewayConfig {
	return config.GatewayConfig{Name: name, Kind: "custom", Endpoint: url, DefaultModel: "m", TrainsOnInputs: trains}
}

var items = []Item{{Repo: "o/r", Number: 7, Title: "Rebase does nothing", Body: "see a.go", Lane: "fixer"}}

func newEngine(rd *reader, rec *recorder, private func(context.Context, string) (bool, error)) *Engine {
	return New(Deps{Reader: rd, RepoPrivate: private, Record: rec.record})
}

func public(context.Context, string) (bool, error)  { return false, nil }
func private(context.Context, string) (bool, error) { return true, nil }

// TestOffByDefault: an absent block, or a mode this build does not
// implement, does nothing at all: no gateway call, no read, no record.
func TestOffByDefault(t *testing.T) {
	g := &gateway{script: triageScript()}
	srv := g.serve(t)
	for name, cfg := range map[string]*config.Config{
		"absent":   {},
		"disabled": cfgWith(false, gw("gw", srv.URL, false)),
		"report-only": func() *config.Config {
			c := cfgWith(true, gw("gw", srv.URL, false))
			c.AdvisoryEngine.Mode = "report-only"
			return c
		}(),
		"nil": nil,
	} {
		rd, rec := &reader{}, &recorder{}
		visibility := 0
		e := newEngine(rd, rec, func(context.Context, string) (bool, error) { visibility++; return false, nil })
		if n := e.Observe(context.Background(), cfg, items); n != 0 {
			t.Fatalf("%s: Observe started %d sessions", name, n)
		}
		e.Wait()
		if g.n() != 0 || rd.reads != 0 || visibility != 0 || len(rec.got) != 0 {
			t.Fatalf("%s: engine did work while off (calls %d reads %d visibility %d records %d)", name, g.n(), rd.reads, visibility, len(rec.got))
		}
	}
	if got := (config.AdvisoryEngineConfig{}).EffectiveMode(); got != config.AdvisoryEngineModeOff {
		t.Fatalf("zero config mode = %q", got)
	}
	if got := (config.AdvisoryEngineConfig{Enabled: true}).EffectiveMode(); got != config.AdvisoryEngineModeShadow {
		t.Fatalf("enabled mode = %q, want shadow", got)
	}
}

// TestShadowResultRecordedNotActedOn: an enabled engine records one grounded
// result per item, through Record only, and does not rerun a seen item.
func TestShadowResultRecordedNotActedOn(t *testing.T) {
	g := &gateway{script: triageScript()}
	rd, rec := &reader{}, &recorder{}
	e := newEngine(rd, rec, public)
	cfg := cfgWith(true, gw("gw", g.serve(t).URL, false))
	if n := e.Observe(context.Background(), cfg, items); n != 1 {
		t.Fatalf("Observe started %d, want 1", n)
	}
	e.Wait()
	if len(rec.got) != 1 {
		t.Fatalf("records = %d, want 1", len(rec.got))
	}
	r := rec.got[0]
	if r.Mode != "shadow" || r.Outcome != hivetool.ToolTaskComplete || r.Verified != 1 || r.Provider != "gw" || r.Lane != "fixer" || r.Error != "" {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Summary, "src/a.go:3") || rd.reads != 1 {
		t.Fatalf("summary %q, reads %d", r.Summary, rd.reads)
	}
	if n := e.Observe(context.Background(), cfg, items); n != 0 {
		t.Fatalf("seen item rerun (%d)", n)
	}
	e.Wait()
	if len(rec.got) != 1 {
		t.Fatalf("records = %d after rerun", len(rec.got))
	}
}

var hiveWriteTools = []string{"issue.comment", "issue.label", "repo.patch_files", "pr.create", "pr.comment"}

// TestNoWriteToolOffered: neither OfferedTools nor a live session ever
// offers a write tool definition.
func TestNoWriteToolOffered(t *testing.T) {
	for _, search := range []bool{true, false} {
		for _, d := range OfferedTools(search) {
			if !toolapprove.IsReadOnly(d.Tool) {
				t.Fatalf("OfferedTools(%v) offers non-read tool %q", search, d.Tool)
			}
		}
	}
	g := &gateway{script: triageScript()}
	e := newEngine(&reader{}, &recorder{}, public)
	e.Observe(context.Background(), cfgWith(true, gw("gw", g.serve(t).URL, false)), items)
	e.Wait()
	offered := g.offeredNames()
	if len(offered) == 0 {
		t.Fatal("session offered no tools")
	}
	for name := range offered {
		if !toolapprove.IsReadOnly(name) {
			t.Fatalf("session offered non-read tool %q", name)
		}
	}
	for _, w := range hiveWriteTools {
		if offered[w] {
			t.Fatalf("session offered write tool %q", w)
		}
	}
}

// TestNoGitHubWriteClient: the engine's dependencies are a read-only Reader,
// a visibility lookup, a sink, a logger and the gateway HTTP client; there
// is no GitHub client field, and the HTTP layer under the Reader refuses
// writes.
func TestNoGitHubWriteClient(t *testing.T) {
	allowed := map[string]bool{"Reader": true, "Searcher": true, "RepoPrivate": true, "Record": true, "Logger": true, "HTTPClient": true}
	typ := reflect.TypeOf(Deps{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !allowed[f.Name] || strings.Contains(f.Type.String(), "github") {
			t.Fatalf("unexpected engine dependency %s %s", f.Name, f.Type)
		}
	}
	reader := reflect.TypeOf((*hivetool.Reader)(nil)).Elem()
	for i := 0; i < reader.NumMethod(); i++ {
		switch reader.Method(i).Name {
		case "Issue", "List", "File", "Checks":
		default:
			t.Fatalf("Reader grew a method %q", reader.Method(i).Name)
		}
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := ReadOnlyHTTPClient(srv.Client())
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(m, srv.URL+"/repos/o/r/issues/7/comments", strings.NewReader("{}"))
		if _, err := c.Do(req); !errors.Is(err, ErrWriteRefused) {
			t.Fatalf("%s: err = %v, want ErrWriteRefused", m, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a write reached the server (%d)", hits.Load())
	}
	resp, err := c.Get(srv.URL + "/repos/o/r")
	if err != nil || hits.Load() != 1 {
		t.Fatalf("GET: %v (hits %d)", err, hits.Load())
	}
	resp.Body.Close()
}

// TestTrainingProvidersSkippedForPrivate: a gateway marked trains_on_inputs
// never sees a private (or unknown-visibility) repository unless the
// operator opts in; with only such gateways the item is skipped.
func TestTrainingProvidersSkippedForPrivate(t *testing.T) {
	unknown := func(context.Context, string) (bool, error) { return false, errors.New("boom") }
	for name, vis := range map[string]func(context.Context, string) (bool, error){"private": private, "unknown": unknown} {
		trainer, clean := &gateway{script: triageScript()}, &gateway{script: triageScript()}
		rec := &recorder{}
		e := newEngine(&reader{}, rec, vis)
		e.Observe(context.Background(), cfgWith(true, gw("trainer", trainer.serve(t).URL, true), gw("clean", clean.serve(t).URL, false)), items)
		e.Wait()
		if trainer.n() != 0 || clean.n() == 0 || rec.got[0].Provider != "clean" || !rec.got[0].Private {
			t.Fatalf("%s: trainer %d clean %d result %+v", name, trainer.n(), clean.n(), rec.got[0])
		}
	}

	trainer := &gateway{script: triageScript()}
	url := trainer.serve(t).URL
	rec := &recorder{}
	e := newEngine(&reader{}, rec, private)
	e.Observe(context.Background(), cfgWith(true, gw("trainer", url, true)), items)
	e.Wait()
	if trainer.n() != 0 || rec.got[0].Skipped == "" {
		t.Fatalf("only-trainer private: calls %d result %+v", trainer.n(), rec.got[0])
	}

	for name, setup := range map[string]struct {
		vis   func(context.Context, string) (bool, error)
		optIn bool
	}{"public": {public, false}, "private opt-in": {private, true}} {
		trainer := &gateway{script: triageScript()}
		cfg := cfgWith(true, gw("trainer", trainer.serve(t).URL, true))
		cfg.AdvisoryEngine.AllowTrainingProvidersForPrivate = setup.optIn
		rec := &recorder{}
		e := newEngine(&reader{}, rec, setup.vis)
		e.Observe(context.Background(), cfg, items)
		e.Wait()
		if trainer.n() == 0 || rec.got[0].Provider != "trainer" {
			t.Fatalf("%s: calls %d result %+v", name, trainer.n(), rec.got[0])
		}
	}
}
