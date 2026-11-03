package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// laneServer is the lane half of the wire contract: it records every
// request (method, URI, body) and answers with canned lane documents so the
// typed lane methods are tested against the contract, not crux/serve.
type laneServer struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
}

func (l *laneServer) handler() http.Handler {
	mux := http.NewServeMux()
	record := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			l.mu.Lock()
			l.requests = append(l.requests, r.Method+" "+r.URL.RequestURI())
			l.bodies = append(l.bodies, string(data))
			l.mu.Unlock()
			h(w, r)
		}
	}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(v)
	}
	expires := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	mux.HandleFunc("GET "+PathLanes, record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []Lane{{ID: "exchange", Owner: "atlas", Groups: map[string][]string{"workers": {"beacon"}}}})
	}))
	mux.HandleFunc("POST "+PathLanes+"/{lane}/publish", record(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("lane") == "full" {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, Error{Error: "lane exchange is full", Code: CodeLaneCapacity})
			return
		}
		writeJSON(w, LanePublished{Sequence: 7, Duplicate: r.PathValue("lane") == "dup"})
	}))
	mux.HandleFunc("GET "+PathLanes+"/{lane}/events", record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, LaneReplay{Cursor: 9, Events: []LaneRecord{{Sequence: 9, Envelope: LaneEnvelope{Lane: "exchange", LanePublish: LanePublish{ID: "e1", Kind: LaneEvent}}}}})
	}))
	mux.HandleFunc("POST "+PathLanes+"/{lane}/groups/{group}/claim", record(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("group") == "idle" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, LaneClaim{LaneRecord: LaneRecord{Sequence: 3}, Receipt: "r-3", Attempt: 2, Expires: expires})
	}))
	mux.HandleFunc("POST "+PathLanes+"/{lane}/groups/{group}/ack", record(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST "+PathLanes+"/{lane}/groups/{group}/nack", record(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("group") == "stale" {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, Error{Error: "receipt is stale", Code: CodeLaneStale})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST "+PathLanes+"/{lane}/groups/{group}/renew", record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, LaneRenewed{Expires: expires})
	}))
	mux.HandleFunc("GET "+PathLanes+"/{lane}/groups/{group}", record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, LaneGroupState{Revision: 4, Cursor: 9, LastSequence: 11, Slots: 2, Recoverable: []LaneReceipt{{Sequence: 10, Receipt: "r-10"}}})
	}))
	mux.HandleFunc("POST "+PathLanes+"/{lane}/groups/{group}/recover", record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, LaneRecovered{ID: "rec-1", Cursor: 11, Retired: []uint64{10}, RecoveredAt: expires})
	}))
	return mux
}

func (l *laneServer) seen() (requests, bodies []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.requests...), append([]string(nil), l.bodies...)
}

func startLaneServer(t *testing.T) (*laneServer, *Client) {
	t.Helper()
	l := &laneServer{}
	srv := httptest.NewServer(l.handler())
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return l, c
}

func TestLanePathsEscapeSegments(t *testing.T) {
	if got := lanePath("a b/c"); got != PathLanes+"/a%20b%2Fc" {
		t.Fatalf("lanePath %q", got)
	}
	if got := laneGroupPath("ex", "wor kers"); got != PathLanes+"/ex/groups/wor%20kers" {
		t.Fatalf("laneGroupPath %q", got)
	}
}

func TestLaneClientRoutesAndDecodes(t *testing.T) {
	l, c := startLaneServer(t)
	ctx := context.Background()

	lanes, err := c.LaneRegistry(ctx)
	if err != nil || len(lanes) != 1 || lanes[0].ID != "exchange" || lanes[0].Groups["workers"][0] != "beacon" {
		t.Fatalf("registry %+v %v", lanes, err)
	}
	pub, err := c.PublishLane(ctx, "ex change", LanePublish{ID: "w1", Kind: LaneWork, Schema: "s", Payload: []byte(`{"x":1}`)})
	if err != nil || pub.Sequence != 7 || pub.Duplicate {
		t.Fatalf("publish %+v %v", pub, err)
	}
	if pub, err = c.PublishLane(ctx, "dup", LanePublish{ID: "w1"}); err != nil || !pub.Duplicate {
		t.Fatalf("duplicate publish %+v %v", pub, err)
	}
	replay, err := c.ReplayLane(ctx, "exchange", 5, 20)
	if err != nil || replay.Cursor != 9 || len(replay.Events) != 1 || replay.Events[0].Envelope.ID != "e1" {
		t.Fatalf("replay %+v %v", replay, err)
	}
	claim, err := c.ClaimLane(ctx, "exchange", "work ers")
	if err != nil || claim == nil || claim.Receipt != "r-3" || claim.Sequence != 3 || claim.Attempt != 2 || claim.Expires.IsZero() {
		t.Fatalf("claim %+v %v", claim, err)
	}
	// 204 means nothing to claim: nil claim, nil error, by contract.
	if idle, err := c.ClaimLane(ctx, "exchange", "idle"); err != nil || idle != nil {
		t.Fatalf("idle claim %+v %v", idle, err)
	}
	receipt := LaneReceipt{Sequence: 3, Receipt: "r-3"}
	if err := c.AckLane(ctx, "exchange", "workers", receipt); err != nil {
		t.Fatalf("ack %v", err)
	}
	if err := c.NackLane(ctx, "exchange", "workers", receipt); err != nil {
		t.Fatalf("nack %v", err)
	}
	renewed, err := c.RenewLane(ctx, "exchange", "workers", receipt)
	if err != nil || renewed.Expires.Year() != 2026 {
		t.Fatalf("renew %+v %v", renewed, err)
	}
	state, err := c.InspectLaneGroup(ctx, "exchange", "workers")
	if err != nil || state.Revision != 4 || state.LastSequence != 11 || state.Slots != 2 || len(state.Recoverable) != 1 {
		t.Fatalf("inspect %+v %v", state, err)
	}
	recovered, err := c.RecoverLaneGroup(ctx, "exchange", "workers", LaneRecover{ID: "rec-1", Revision: 4, CursorThrough: 11, Missing: []uint64{10}})
	if err != nil || recovered.ID != "rec-1" || recovered.Cursor != 11 || len(recovered.Retired) != 1 {
		t.Fatalf("recover %+v %v", recovered, err)
	}

	requests, bodies := l.seen()
	wantRequests := []string{
		"GET /v1/lanes",
		"POST /v1/lanes/ex%20change/publish",
		"POST /v1/lanes/dup/publish",
		"GET /v1/lanes/exchange/events?cursor=5&limit=20",
		"POST /v1/lanes/exchange/groups/work%20ers/claim",
		"POST /v1/lanes/exchange/groups/idle/claim",
		"POST /v1/lanes/exchange/groups/workers/ack",
		"POST /v1/lanes/exchange/groups/workers/nack",
		"POST /v1/lanes/exchange/groups/workers/renew",
		"GET /v1/lanes/exchange/groups/workers",
		"POST /v1/lanes/exchange/groups/workers/recover",
	}
	if strings.Join(requests, "\n") != strings.Join(wantRequests, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(requests, "\n"), strings.Join(wantRequests, "\n"))
	}
	// Bodies: GETs and claim send nothing; publish carries the envelope,
	// ack/nack/renew the receipt, recover the fence.
	for i, want := range []string{"", `"id":"w1","kind":"work","schema":"s","payload":"eyJ4IjoxfQ=="`, `"id":"w1"`, "", "", "",
		`{"sequence":3,"receipt":"r-3"}`, `{"sequence":3,"receipt":"r-3"}`, `{"sequence":3,"receipt":"r-3"}`, "",
		`"id":"rec-1","revision":4,"cursor_through":11,"expired":null,"missing":[10]`} {
		if want == "" {
			if bodies[i] != "" {
				t.Errorf("request %d %s: unexpected body %q", i, requests[i], bodies[i])
			}
			continue
		}
		if !strings.Contains(bodies[i], want) {
			t.Errorf("request %d %s: body %q lacks %q", i, requests[i], bodies[i], want)
		}
	}
}

func TestLaneClientSurfacesLaneErrorCodes(t *testing.T) {
	_, c := startLaneServer(t)
	ctx := context.Background()

	_, err := c.PublishLane(ctx, "full", LanePublish{ID: "w2"})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusConflict || se.Body.Code != CodeLaneCapacity || err.Error() != "lane exchange is full" {
		t.Fatalf("publish capacity error: %v (%+v)", err, se)
	}
	err = c.NackLane(ctx, "exchange", "stale", LaneReceipt{Sequence: 1, Receipt: "old"})
	if !errors.As(err, &se) || se.Body.Code != CodeLaneStale {
		t.Fatalf("stale nack error: %v (%+v)", err, se)
	}
	// An unreachable server is a transport error, not a StatusError.
	dead, err := NewClient("127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dead.LaneRegistry(ctx); !errors.Is(err, ErrUnreachable) || errors.As(err, &se) {
		t.Fatalf("unreachable registry: %v", err)
	}
}
