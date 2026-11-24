package meetings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeQuorumServer is a contract-shaped Quorum: MCP over POST /mcp and
// SSE over GET /events, with the bearer token check.
type fakeQuorumServer struct {
	t          *testing.T
	token      string
	mu         sync.Mutex
	calls      []string
	args       []map[string]any
	results    map[string]string // tool -> envelope JSON
	sse        bool              // answer tools/call as an event stream
	structured bool              // put the envelope in structuredContent
	expire     int               // answer the next N session requests 404
	events     map[string][]string
	inits      int
	afterSeq   string
}

func newFakeQuorumServer(t *testing.T) (*fakeQuorumServer, *httptest.Server) {
	f := &fakeQuorumServer{t: t, token: "tok", results: map[string]string{}, events: map[string][]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeQuorumServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/events":
		f.serveEvents(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/mcp":
		f.serveMCP(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeQuorumServer) serveEvents(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("meeting_id")
	if id == "" {
		id = "*"
	}
	f.mu.Lock()
	f.afterSeq = r.URL.Query().Get("after_seq")
	evs, ok := f.events[id]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, ": keepalive\n\nevent: message\n")
	for _, e := range evs {
		_, _ = io.WriteString(w, "data: "+e+"\n\n")
	}
}

func (f *fakeQuorumServer) serveMCP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Method != "initialize" && r.Header.Get(mcpSessionHeader) != "s1" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Method != "initialize" && f.expire > 0 {
		f.expire--
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch req.Method {
	case "initialize":
		f.inits++
		w.Header().Set(mcpSessionHeader, "s1")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26"}}`, req.ID)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		f.calls = append(f.calls, req.Params.Name)
		f.args = append(f.args, req.Params.Arguments)
		env, ok := f.results[req.Params.Name]
		if !ok {
			env = `{"ok":true}`
		}
		var result string
		if f.structured {
			result = fmt.Sprintf(`{"content":[],"structuredContent":%s}`, env)
		} else {
			b, _ := json.Marshal(env)
			result = fmt.Sprintf(`{"content":[{"type":"text","text":%s}]}`, b)
		}
		msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
		if f.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\ndata: %s\n\n", msg)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, msg)
	case "rpc-error":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"nope"}}`, req.ID)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func TestClientToolsJSONAndSSE(t *testing.T) {
	f, srv := newFakeQuorumServer(t)
	c := NewClient(srv.URL+"/", "tok", nil)
	ctx := context.Background()
	f.results["meeting.join"] = `{"ok":true,"meeting_id":"q1","platform":"google_meet","state":"joining"}`
	res, err := c.Join(ctx, JoinArgs{URL: "https://meet.google.com/abc", DisplayName: "hive (notes)"})
	if err != nil || res.MeetingID != "q1" || res.State != StateJoining {
		t.Fatalf("Join = %+v, %v", res, err)
	}
	f.sse = true
	f.results["meeting.leave"] = `{"ok":true,"state":"ended"}`
	if st, err := c.Leave(ctx, "q1"); err != nil || st != StateEnded {
		t.Fatalf("Leave = %v, %v", st, err)
	}
	f.sse = false
	f.structured = true
	f.results["meeting.status"] = `{"ok":true,"state":"live","participants":[{"id":"u1","name":"Ana"}]}`
	if st, err := c.Status(ctx, "q1"); err != nil || st.State != StateLive || st.Participants[0].Name != "Ana" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	f.results["transcript.read"] = `{"ok":true,"segments":[{"seq":1,"text":"hi"}],"next_seq":2}`
	if tr, err := c.TranscriptRead(ctx, "q1", 0); err != nil || tr.NextSeq != 2 || len(tr.Segments) != 1 {
		t.Fatalf("TranscriptRead = %+v, %v", tr, err)
	}
	if _, err := c.TranscriptRead(ctx, "q1", 5); err != nil {
		t.Fatal(err)
	}
	if err := c.ChatPost(ctx, "q1", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.Say(ctx, "q1", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "q1"); err != nil {
		t.Fatal(err)
	}
	want := "meeting.join,meeting.leave,meeting.status,transcript.read,transcript.read,chat.post,say,meeting.delete"
	if got := strings.Join(f.calls, ","); got != want {
		t.Fatalf("calls %s", got)
	}
	if f.args[4]["after_seq"] != float64(5) || f.args[5]["text"] != "hello" || f.args[0]["display_name"] != "hive (notes)" {
		t.Fatalf("args %+v", f.args)
	}
	if f.inits != 1 {
		t.Fatalf("initialize ran %d times", f.inits)
	}
}

func TestClientToolErrors(t *testing.T) {
	f, srv := newFakeQuorumServer(t)
	c := NewClient(srv.URL, "tok", nil)
	ctx := context.Background()
	f.results["chat.post"] = `{"ok":false,"error":{"code":"not_live","message":"meeting not live"}}`
	err := c.ChatPost(ctx, "q1", "x")
	if !IsCode(err, CodeNotLive) || !strings.Contains(err.Error(), "chat.post") {
		t.Fatalf("err = %v", err)
	}
	f.results["say"] = `{"ok":false}`
	if err := c.Say(ctx, "q1", "x"); !IsCode(err, CodeUpstreamError) {
		t.Fatalf("err = %v", err)
	}
	f.results["meeting.delete"] = `not json`
	if err := c.Delete(ctx, "q1"); err == nil || !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("err = %v", err)
	}
	f.results["meeting.join"] = `{"ok":true,"meeting_id":7}`
	if _, err := c.Join(ctx, JoinArgs{}); err == nil {
		t.Fatal("want a decode error")
	}
	if IsCode(errors.New("x"), CodeNotLive) {
		t.Fatal("plain errors carry no code")
	}
	if _, err := c.request(ctx, "rpc-error", nil, nil); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("rpc error = %v", err)
	}
	if _, err := c.request(ctx, "teapot", nil, nil); err == nil || !strings.Contains(err.Error(), "418") {
		t.Fatalf("http error = %v", err)
	}
	if err := decodeToolResult("x", json.RawMessage(`[`), nil); err == nil {
		t.Fatal("want a decode error")
	}
}

func TestClientReinitializesExpiredSession(t *testing.T) {
	f, srv := newFakeQuorumServer(t)
	c := NewClient(srv.URL, "tok", nil)
	ctx := context.Background()
	if err := c.ChatPost(ctx, "q1", "a"); err != nil {
		t.Fatal(err)
	}
	f.expire = 1
	if err := c.ChatPost(ctx, "q1", "b"); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if f.inits != 2 {
		t.Fatalf("inits = %d, want a re-initialize", f.inits)
	}
}

func TestClientRechecksJoinAfterSessionExpires(t *testing.T) {
	f, srv := newFakeQuorumServer(t)
	c := NewClient(srv.URL, "tok", nil)
	ctx := context.Background()
	if err := c.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.expire = 1
	f.mu.Unlock()

	checks := 0
	_, err := c.Join(ctx, JoinArgs{beforeDispatch: func() error {
		checks++
		if checks == 2 {
			return ErrJoinSuperseded
		}
		return nil
	}})
	if !errors.Is(err, ErrJoinSuperseded) || checks != 2 {
		t.Fatalf("Join = %v after %d checks, want superseded on retry", err, checks)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inits != 2 || len(f.calls) != 0 {
		t.Fatalf("initialized %d times and sent accepted calls %v", f.inits, f.calls)
	}
}

func TestClientTransportErrors(t *testing.T) {
	_, srv := newFakeQuorumServer(t)
	bad := NewClient(srv.URL, "wrong", nil)
	if err := bad.ChatPost(context.Background(), "q1", "x"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	srv.Close()
	down := NewClient(srv.URL, "tok", nil)
	if err := down.ChatPost(context.Background(), "q1", "x"); err == nil {
		t.Fatal("want a transport error")
	}
	if err := down.Events(context.Background(), "q1", 0, func(Event) bool { return true }); err == nil {
		t.Fatal("want a transport error")
	}
	if err := NewClient("http://bad host", "t", nil).ChatPost(context.Background(), "q", "x"); err == nil {
		t.Fatal("want a request error")
	}
	if err := NewClient("http://bad host", "t", nil).Events(context.Background(), "q", 0, nil); err == nil {
		t.Fatal("want a request error")
	}
}

func TestClientResponseShapes(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"bad json": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(mcpSessionHeader, "s")
			_, _ = io.WriteString(w, "{")
		},
		"stream without reply": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":999}\n\n")
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if err := NewClient(srv.URL, "t", nil).ChatPost(context.Background(), "q", "x"); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestClientEvents(t *testing.T) {
	f, srv := newFakeQuorumServer(t)
	f.events["q1"] = []string{
		`{"type":"state","meeting_id":"q1","state":"live"}`,
		`not json`,
		`{"type":"segment","meeting_id":"q1","seq":1,"speaker":{"id":"a","name":"Ana"},"text":"hi","start":1.5,"end":2,"final":true}`,
		`{"type":"chat","meeting_id":"q1","seq":2,"author":{"name":"Bo"},"text":"yo","at":"2026-11-15T10:00:00Z"}`,
	}
	c := NewClient(srv.URL, "tok", nil)
	var got []Event
	err := c.Events(context.Background(), "q1", 0, func(ev Event) bool { got = append(got, ev); return true })
	if err == nil || !strings.Contains(err.Error(), "stream ended") {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 3 || got[0].State != StateLive || got[1].Who() != "Ana" || got[2].Who() != "Bo" || !got[1].Final {
		t.Fatalf("events %+v", got)
	}
	if !strings.Contains(string(got[1].Raw), `"start":1.5`) {
		t.Fatalf("raw not kept: %s", got[1].Raw)
	}
	n := 0
	if err := c.Events(context.Background(), "q1", 7, func(Event) bool { n++; return false }); err != nil || n != 1 || f.afterSeq != "7" {
		t.Fatalf("stop: %v, %d", err, n)
	}
	f.events["*"] = []string{`{"type":"state","meeting_id":"q9","state":"requested","platform":"discord","guild_id":"g","channel_id":"c","requested_by":{"id":"u1","name":"Ana"}}`}
	var req Event
	_ = c.Events(context.Background(), "", 0, func(ev Event) bool { req = ev; return false })
	if req.RequestedBy == nil || req.RequestedBy.Name != "Ana" || req.GuildID != "g" || f.afterSeq != "" {
		t.Fatalf("request event %+v", req)
	}
	if err := c.Events(context.Background(), "missing", 0, nil); !errors.Is(err, ErrStreamGone) {
		t.Fatalf("missing = %v", err)
	}
	if err := NewClient(srv.URL, "wrong", nil).Events(context.Background(), "q1", 0, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthorized = %v", err)
	}
}

func TestReadSSE(t *testing.T) {
	var got []string
	in := "data: a\ndata: b\nid: 1\n\n:comment\ndata:c\n\ndata: tail"
	if err := readSSE(strings.NewReader(in), func(d []byte) bool { got = append(got, string(d)); return true }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "a\nb|c|tail" {
		t.Fatalf("got %q", got)
	}
	long := "data: " + strings.Repeat("x", maxSSELine+1) + "\n"
	if err := readSSE(strings.NewReader(long), func([]byte) bool { return true }); err == nil {
		t.Fatal("want a line-too-long error")
	}
	if (Event{}).Who() != "" {
		t.Fatal("empty event has no author")
	}
}

func TestClientChatPostConfirmed(t *testing.T) {
	f, srv := newFakeQuorumServer(t)
	c := NewClient(srv.URL, "tok", nil)
	ctx := context.Background()
	f.results["chat.post"] = `{"ok":true,"posted_at":"2026-11-17T01:52:00Z","delivery":"confirmed"}`
	if ok, err := c.ChatPostConfirmed(ctx, "q1", "notice"); err != nil || !ok {
		t.Fatalf("confirmed = %v, %v", ok, err)
	}
	f.mu.Lock()
	sent := f.args[len(f.args)-1]
	f.mu.Unlock()
	if sent["confirm"] != true || sent["text"] != "notice" || sent["meeting_id"] != "q1" {
		t.Fatalf("args %+v", sent)
	}
	f.results["chat.post"] = `{"ok":true,"posted_at":"2026-11-17T01:52:00Z","delivery":"unconfirmed"}`
	if ok, err := c.ChatPostConfirmed(ctx, "q1", "notice"); err != nil || ok {
		t.Fatalf("unconfirmed = %v, %v", ok, err)
	}
	// A Quorum older than 0.2.2 answers no delivery: not known to be posted.
	f.results["chat.post"] = `{"ok":true,"posted_at":"2026-11-17T01:52:00Z"}`
	if ok, err := c.ChatPostConfirmed(ctx, "q1", "notice"); err != nil || ok {
		t.Fatalf("legacy = %v, %v", ok, err)
	}
	f.results["chat.post"] = `{"ok":false,"error":{"code":"platform_unavailable","message":"the chat line was not posted: no_composer"}}`
	if _, err := c.ChatPostConfirmed(ctx, "q1", "notice"); !IsCode(err, CodePlatformUnavailable) {
		t.Fatalf("failed send = %v", err)
	}
}
