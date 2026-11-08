package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/gates"
	"github.com/hivecommons/hive/crux/lanes"
)

type gateClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *gateClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *gateClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type gateFixture struct {
	t      *testing.T
	dir    string
	clock  *gateClock
	auth   *lanes.Authenticator
	tokens map[string]string
	s      *Server
	h      http.Handler
}

// newGateFixture is a token-bearing server named "kelly" with a gate
// service and two lane projects (atlas, beacon) besides the operator.
func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	tokens := map[string]string{"atlas": strings.Repeat("a", 32), "beacon": strings.Repeat("b", 32), "operator": "agent-admin"}
	cfg := lanes.Config{Principals: []lanes.Principal{{Project: "atlas", TokenEnv: "ATLAS"}, {Project: "beacon", TokenEnv: "BEACON"}}}
	auth, err := lanes.Bind(cfg, func(env string) string { return tokens[strings.ToLower(env)] }, tokens["operator"])
	if err != nil {
		t.Fatal(err)
	}
	f := &gateFixture{t: t, dir: filepath.Join(t.TempDir(), gates.DirName), auth: auth, tokens: tokens,
		clock: &gateClock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}}
	f.restart()
	return f
}

// restart is a new crux serve process on the same work dir.
func (f *gateFixture) restart() {
	f.t.Helper()
	svc, err := gates.New(gates.Config{Dir: f.dir, Now: f.clock.now})
	if err != nil {
		f.t.Fatal(err)
	}
	f.s = New(newFakeRuntime())
	f.s.Token, f.s.LaneAuth, f.s.OperatorName, f.s.Gates = f.tokens["operator"], f.auth, "kelly", svc
	f.h = f.s.Handler()
}

// call sends method path as who and decodes a 2xx body into out.
func (f *gateFixture) call(who, method, path string, body, out any) (int, api.Error) {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+f.tokens[who])
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	var e api.Error
	if rec.Code >= 300 {
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
	} else if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			f.t.Fatalf("decode %s %s: %v: %s", method, path, err, rec.Body)
		}
	}
	return rec.Code, e
}

func TestGateRoutesOpenAnswerAcrossRestart(t *testing.T) {
	f := newGateFixture(t)
	req := api.GateRequest{Subject: "Approve experiment budget for cache-17", Context: json.RawMessage(`{"budget_usd":40}`), OpenedBy: "nous-campaign-42"}
	var g api.Gate
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, req, &g); code != http.StatusCreated {
		t.Fatalf("open: %d %+v", code, e)
	}
	if g.Status != api.GateWaiting || g.OpenedByPrincipal != "project:atlas" || g.OpenedBy != "nous-campaign-42" || strings.Join(g.Choices, ",") != "approve,deny" || g.Timeout != "0" {
		t.Fatalf("opened = %+v", g)
	}
	var again api.Gate
	if code, _ := f.call("atlas", http.MethodPost, api.PathGates, req, &again); code != http.StatusOK || again.ID != g.ID {
		t.Fatalf("reopen: %d %+v", code, again)
	}

	// Visibility: beacon neither reads nor lists atlas's gate; the operator does.
	if code, e := f.call("beacon", http.MethodGet, api.GatePath(g.ID), nil, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon get: %d %+v", code, e)
	}
	var list api.GateList
	if code, _ := f.call("beacon", http.MethodGet, api.PathGates, nil, &list); code != http.StatusOK || len(list.Gates) != 0 {
		t.Fatalf("beacon list: %d %+v", code, list)
	}
	if code, _ := f.call("operator", http.MethodGet, api.PathGates+"?status=waiting", nil, &list); code != http.StatusOK || len(list.Gates) != 1 {
		t.Fatalf("operator list: %d %+v", code, list)
	}
	if code, e := f.call("operator", http.MethodGet, api.PathGates+"?status=pending", nil, nil); code != http.StatusBadRequest || e.Code != api.CodeBadRequest {
		t.Fatalf("bad status filter: %d %+v", code, e)
	}

	// Only the operator answers, and only with an offered choice.
	if code, e := f.call("atlas", http.MethodPost, api.GateAnswerPath(g.ID), api.GateAnswerRequest{Answer: "approve"}, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("project answer: %d %+v", code, e)
	}
	if code, e := f.call("operator", http.MethodPost, api.GateAnswerPath(g.ID), api.GateAnswerRequest{Answer: "looks fine to me"}, nil); code != http.StatusUnprocessableEntity || e.Code != api.CodeGateInvalidChoice {
		t.Fatalf("free-text answer: %d %+v", code, e)
	}

	f.restart()
	var answered api.Gate
	if code, e := f.call("operator", http.MethodPost, api.GateAnswerPath(g.ID), api.GateAnswerRequest{Answer: "approve", Note: "within budget"}, &answered); code != http.StatusOK {
		t.Fatalf("answer after restart: %d %+v", code, e)
	}
	if answered.Status != api.GateAnswered || answered.Answer != "approve" || answered.AnsweredBy != "kelly" || answered.AnsweredByPrincipal != "operator:kelly" || answered.Note != "within budget" {
		t.Fatalf("answered = %+v", answered)
	}
	var reopened api.Gate
	if code, _ := f.call("atlas", http.MethodPost, api.PathGates, req, &reopened); code != http.StatusOK || reopened.ID != g.ID || reopened.Answer != "approve" {
		t.Fatalf("reopen after answer: %d %+v", code, reopened)
	}
	var waited api.Gate
	if code, _ := f.call("atlas", http.MethodGet, api.GateWaitPath(g.ID)+"?timeout=1s", nil, &waited); code != http.StatusOK || waited.Status != api.GateAnswered {
		t.Fatalf("wait: %d %+v", code, waited)
	}
	if code, e := f.call("operator", http.MethodPost, api.GateAnswerPath(g.ID), api.GateAnswerRequest{Answer: "deny"}, nil); code != http.StatusConflict || e.Code != api.CodeGateClosed {
		t.Fatalf("second answer: %d %+v", code, e)
	}
	if code, e := f.call("operator", http.MethodGet, api.GatePath("gate_missing"), nil, nil); code != http.StatusNotFound || e.Code != api.CodeNotFound {
		t.Fatalf("unknown gate: %d %+v", code, e)
	}
}

func TestGateSelfAnswerNeedsAuditedOverride(t *testing.T) {
	f := newGateFixture(t)
	events := f.s.subscribe()
	defer f.s.unsubscribe(events)
	var g api.Gate
	if code, e := f.call("operator", http.MethodPost, api.PathGates, api.GateRequest{Subject: "self-serve", Context: json.RawMessage(`{"secret_plan":"x"}`)}, &g); code != http.StatusCreated {
		t.Fatalf("open: %d %+v", code, e)
	}
	if code, e := f.call("operator", http.MethodPost, api.GateAnswerPath(g.ID), api.GateAnswerRequest{Answer: "approve"}, nil); code != http.StatusForbidden || e.Code != api.CodeGateSelfAnswer {
		t.Fatalf("self answer: %d %+v", code, e)
	}
	var answered api.Gate
	if code, e := f.call("operator", http.MethodPost, api.GateAnswerPath(g.ID), api.GateAnswerRequest{Answer: "approve", AsOperator: true}, &answered); code != http.StatusOK || !answered.OperatorOverride {
		t.Fatalf("override: %d %+v %+v", code, e, answered)
	}
	var types []string
	for len(events) > 0 {
		ev := <-events
		types = append(types, ev.Type)
		if strings.HasPrefix(ev.Type, "gate.") && strings.Contains(string(ev.Data), "secret_plan") {
			t.Fatalf("event %s carries the context: %s", ev.Type, ev.Data)
		}
		if ev.Type == api.EventAudit && !strings.Contains(string(ev.Data), "gate.operator_override") {
			t.Fatalf("audit = %s", ev.Data)
		}
	}
	if strings.Join(types, ",") != api.EventGateOpened+","+api.EventGateAnswered+","+api.EventAudit {
		t.Fatalf("events = %v", types)
	}
}

func TestGateContextLimitAndExpiry(t *testing.T) {
	f := newGateFixture(t)
	big := json.RawMessage(`{"blob":"` + strings.Repeat("x", gates.MaxContextBytes) + `"}`)
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "huge", Context: big}, nil); code != http.StatusRequestEntityTooLarge || e.Code != api.CodeGateContextTooLarge {
		t.Fatalf("large context: %d %+v", code, e)
	}
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "when?", Timeout: "soon"}, nil); code != http.StatusBadRequest || e.Code != api.CodeBadRequest {
		t.Fatalf("bad timeout: %d %+v", code, e)
	}
	var g api.Gate
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "quick", Timeout: "1m"}, &g); code != http.StatusCreated || g.ExpiresAt == nil {
		t.Fatalf("open: %d %+v %+v", code, e, g)
	}
	f.clock.advance(2 * time.Minute)
	var expired api.Gate
	if code, _ := f.call("atlas", http.MethodGet, api.GateWaitPath(g.ID), nil, &expired); code != http.StatusOK || expired.Status != api.GateExpired || expired.Answer != "" {
		t.Fatalf("expired: %d %+v", code, expired)
	}
	if code, e := f.call("operator", http.MethodPost, api.GateAnswerPath(g.ID), api.GateAnswerRequest{Answer: "deny"}, nil); code != http.StatusConflict || e.Code != api.CodeGateClosed {
		t.Fatalf("answer after expiry: %d %+v", code, e)
	}
	if code, e := f.call("atlas", http.MethodGet, api.GateWaitPath(g.ID)+"?timeout=-1s", nil, nil); code != http.StatusBadRequest || e.Code != api.CodeBadRequest {
		t.Fatalf("bad wait timeout: %d %+v", code, e)
	}
}

func TestGateRoutesWithoutService(t *testing.T) {
	f := newGateFixture(t)
	f.s.Gates = nil
	f.h = f.s.Handler()
	if code, e := f.call("operator", http.MethodGet, api.PathGates, nil, nil); code != http.StatusNotImplemented || e.Code != api.CodeNotSupported {
		t.Fatalf("no service: %d %+v", code, e)
	}
}
