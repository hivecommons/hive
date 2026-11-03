package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/lanes"
	"github.com/nats-io/nats-server/v2/server"
)

func laneAPIFixtureConfig(t *testing.T, change func(*lanes.Config)) (*Server, map[string]*api.Client, *httptest.Server) {
	t.Helper()
	dir := filepath.Join(".", fmt.Sprintf(".lane-fixture-%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: dir, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(time.Second * 10) {
		ns.Shutdown()
		t.Fatal("NATS not ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	cfg := lanes.Config{URL: ns.ClientURL(),
		Principals: []lanes.Principal{{Project: "atlas", TokenEnv: "ATLAS"}, {Project: "beacon", TokenEnv: "BEACON"}, {Project: "outsider", TokenEnv: "OUTSIDER"}},
		Lanes: []api.Lane{{ID: "exchange", Owner: "atlas", Publishers: []string{"atlas", "beacon"}, Subscribers: []string{"atlas", "beacon"},
			Groups:     map[string][]string{"workers": {"atlas", "beacon"}, "beacon-only": {"beacon"}},
			Recoverers: map[string][]string{"workers": {"atlas"}}}}}
	cfg.Defaults()
	cfg.RetryDelay, cfg.MaxRetryDelay = lanes.MinRetryDelay, lanes.MinRetryDelay
	if change != nil {
		change(&cfg)
	}
	broker, err := lanes.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	tokens := map[string]string{"ATLAS": strings.Repeat("a", 32), "BEACON": strings.Repeat("b", 32), "OUTSIDER": strings.Repeat("c", 32)}
	auth, err := lanes.Bind(cfg, func(name string) string { return tokens[name] }, "agent-admin")
	if err != nil {
		t.Fatal(err)
	}
	s := New(newFakeRuntime())
	s.Token, s.Lanes, s.LaneAuth = "agent-admin", broker, auth
	httpServer := httptest.NewServer(s.Handler())
	t.Cleanup(httpServer.Close)
	clients := map[string]*api.Client{}
	for project, env := range map[string]string{"atlas": "ATLAS", "beacon": "BEACON", "outsider": "OUTSIDER", "admin": ""} {
		token := tokens[env]
		if project == "admin" {
			token = s.Token
		}
		c, err := api.NewClient(httpServer.URL, token)
		if err != nil {
			t.Fatal(err)
		}
		clients[project] = c
	}
	return s, clients, httpServer
}

func laneAPIFixture(t *testing.T) (*Server, map[string]*api.Client, *httptest.Server) {
	return laneAPIFixtureConfig(t, nil)
}

func requireLaneStatus(t *testing.T, err error, status int) {
	t.Helper()
	var se *api.StatusError
	if !errors.As(err, &se) || se.Status != status {
		t.Fatalf("wanted HTTP %d, got %v", status, err)
	}
}

func TestTwoForgeFreeProjectsExchangeThroughAPI(t *testing.T) {
	_, clients, _ := laneAPIFixture(t)
	ctx := context.Background()
	for _, project := range []string{"atlas", "beacon"} {
		registry, err := clients[project].LaneRegistry(ctx)
		if err != nil || len(registry) != 1 {
			t.Fatalf("registry: %+v %v", registry, err)
		}
		if _, err := clients[project].PublishLane(ctx, "exchange", api.LanePublish{
			ID: project + "-event", Kind: api.LaneEvent, Schema: "measurement-v1", Payload: []byte(`{"temperature":21}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []string{"atlas", "beacon"} {
		page, err := clients[project].ReplayLane(ctx, "exchange", 0, 8)
		if err != nil || len(page.Events) != 2 {
			t.Fatalf("fanout %+v %v", page, err)
		}
		if page.Events[0].Envelope.Project != "atlas" || page.Events[1].Envelope.Project != "beacon" {
			t.Fatal("project identity not bound to authentication")
		}
	}
	work := api.LanePublish{ID: "summarize", Kind: api.LaneWork, Schema: "summary-v1", Payload: []byte(`{}`)}
	pub, err := clients["atlas"].PublishLane(ctx, "exchange", work)
	if err != nil {
		t.Fatal(err)
	}
	dup, err := clients["atlas"].PublishLane(ctx, "exchange", work)
	if err != nil || !dup.Duplicate || dup.Sequence != pub.Sequence {
		t.Fatalf("publish duplicate %+v %v", dup, err)
	}
	work.Payload = []byte("changed")
	_, err = clients["atlas"].PublishLane(ctx, "exchange", work)
	requireLaneStatus(t, err, http.StatusConflict)
	c, err := clients["beacon"].ClaimLane(ctx, "exchange", "workers")
	if err != nil || c == nil {
		t.Fatalf("claim %+v %v", c, err)
	}
	empty, err := clients["atlas"].ClaimLane(ctx, "exchange", "workers")
	if err != nil || empty != nil {
		t.Fatalf("204 claim %+v %v", empty, err)
	}
	r := api.LaneReceipt{Sequence: c.Sequence, Receipt: c.Receipt}
	if _, err := clients["beacon"].RenewLane(ctx, "exchange", "workers", r); err != nil {
		t.Fatal(err)
	}
	if err := clients["beacon"].NackLane(ctx, "exchange", "workers", r); err != nil {
		t.Fatal(err)
	}
	state, err := clients["beacon"].InspectLaneGroup(ctx, "exchange", "workers")
	if err != nil || len(state.Retries) != 1 {
		t.Fatalf("retry state %+v %v", state, err)
	}
	timer := time.NewTimer(time.Until(state.Retries[0].AvailableAt))
	<-timer.C
	redelivery, err := clients["atlas"].ClaimLane(ctx, "exchange", "workers")
	if err != nil || redelivery == nil || redelivery.Attempt != 2 {
		t.Fatalf("redelivery %+v %v", redelivery, err)
	}
	requireLaneStatus(t, clients["beacon"].AckLane(ctx, "exchange", "workers", r), http.StatusConflict)
	requireLaneStatus(t, clients["beacon"].AckLane(ctx, "exchange", "beacon-only", r), http.StatusConflict)
	r = api.LaneReceipt{Sequence: redelivery.Sequence, Receipt: redelivery.Receipt}
	for range 2 {
		if err := clients["atlas"].AckLane(ctx, "exchange", "workers", r); err != nil {
			t.Fatal(err)
		}
	}
	other, err := clients["beacon"].ClaimLane(ctx, "exchange", "beacon-only")
	if err != nil || other == nil || other.Sequence != pub.Sequence {
		t.Fatalf("per-group delivery %+v %v", other, err)
	}
}

func TestLaneAuthDoesNotInheritAgentOrForgeAuthority(t *testing.T) {
	s, clients, hs := laneAPIFixture(t)
	ctx := context.Background()
	requireLaneStatus(t, clients["atlas"].Do(ctx, http.MethodGet, api.PathAgents, nil, nil), http.StatusUnauthorized)
	requireLaneStatus(t, clients["admin"].Do(ctx, http.MethodGet, api.PathLanes, nil, nil), http.StatusUnauthorized)
	if err := clients["admin"].Do(ctx, http.MethodGet, api.PathAgents, nil, nil); err != nil {
		t.Fatal("legacy admin regressed", err)
	}
	registry, err := clients["outsider"].LaneRegistry(ctx)
	if err != nil || len(registry) != 0 {
		t.Fatal("outsider can see registry", err)
	}
	_, err = clients["outsider"].ReplayLane(ctx, "exchange", 0, 1)
	requireLaneStatus(t, err, http.StatusForbidden)
	_, err = clients["outsider"].ClaimLane(ctx, "exchange", "workers")
	requireLaneStatus(t, err, http.StatusForbidden)
	_, err = clients["outsider"].PublishLane(ctx, "exchange", api.LanePublish{ID: "x", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("x")})
	requireLaneStatus(t, err, http.StatusForbidden)
	request, err := http.NewRequest(http.MethodPost, hs.URL+api.PathLanes+"/exchange/publish", strings.NewReader(`{"id":"x","kind":"event","schema":"v1","payload":"eA==","project":"atlas"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("c", 32))
	request.Header.Set("X-Project", "atlas")
	request.Header.Set("X-GitHub-Repository", "irrelevant/forged")
	response, err := hs.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged publisher accepted: %d", response.StatusCode)
	}
	// A transport-local request without a project token still has no lane principal.
	request = httptest.NewRequest(http.MethodGet, api.PathLanes, nil)
	request = request.WithContext(context.WithValue(request.Context(), networkKey, networkUnix))
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatal("unix socket inherited lane permission")
	}
	requireLaneStatus(t, clients["atlas"].Do(ctx, http.MethodPost, api.PathLanes, map[string]string{"owner": "atlas"}, nil), http.StatusMethodNotAllowed)
}

func TestLaneMalformedAndCancellationPaths(t *testing.T) {
	_, clients, hs := laneAPIFixture(t)
	ctx := context.Background()
	for _, query := range []string{"cursor=-1", "cursor=no", "limit=no", "limit=0", "limit=999"} {
		err := clients["atlas"].Do(ctx, http.MethodGet, api.PathLanes+"/exchange/events?"+query, nil, nil)
		var se *api.StatusError
		if !errors.As(err, &se) || (se.Status != http.StatusBadRequest && se.Status != http.StatusConflict) {
			t.Fatalf("%s: %v", query, err)
		}
	}
	requireLaneStatus(t, clients["atlas"].AckLane(ctx, "exchange", "workers", api.LaneReceipt{}), http.StatusBadRequest)
	_, err := clients["atlas"].PublishLane(ctx, "exchange", api.LanePublish{})
	requireLaneStatus(t, err, http.StatusBadRequest)
	err = clients["atlas"].Do(ctx, http.MethodPost, api.PathLanes+"/exchange/publish", json.RawMessage(`{"id":"x"} {}`), nil)
	if err == nil {
		t.Fatal("malformed request accepted")
	}
	for _, body := range []string{"{", "{} {}"} {
		request, err := http.NewRequest(http.MethodPost, hs.URL+api.PathLanes+"/exchange/publish", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
		response, err := hs.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatal("invalid raw lane JSON accepted")
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := clients["atlas"].ReplayLane(ctx, "exchange", 0, 1); err == nil {
		t.Fatal("canceled request succeeded")
	}
}

func TestLaneFailureHTTPClassification(t *testing.T) {
	s := New(newFakeRuntime())
	for _, test := range []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{lanes.ErrInvalid, http.StatusBadRequest, api.CodeLaneInvalid, "invalid lane request"},
		{lanes.ErrForbidden, http.StatusForbidden, api.CodeLaneForbidden, "lane capability denied"},
		{lanes.ErrConflict, http.StatusConflict, api.CodeLaneConflict, "lane state conflict"},
		{lanes.ErrCursor, http.StatusConflict, api.CodeLaneCursor, "invalid or expired lane cursor"},
		{lanes.ErrStale, http.StatusConflict, api.CodeLaneStale, "stale lane receipt"},
		{lanes.ErrCapacity, http.StatusTooManyRequests, api.CodeLaneCapacity, "lane capacity exhausted"},
		{lanes.ErrUnavailable, http.StatusServiceUnavailable, api.CodeLaneUnavailable, "lane broker unavailable"},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, api.CodeLaneTimeout, "lane operation timed out"},
		{context.Canceled, http.StatusRequestTimeout, api.CodeLaneCanceled, "lane operation canceled"},
		{errors.New("dummy-backend-detail"), http.StatusServiceUnavailable, api.CodeLaneUnavailable, "lane broker unavailable"},
	} {
		t.Run(test.err.Error(), func(t *testing.T) {
			for _, err := range []error{test.err, fmt.Errorf("%w: tls://dummy-user:dummy-password@broker.example:4222", test.err)} {
				w := httptest.NewRecorder()
				s.writeLaneError(w, err)
				want := fmt.Sprintf("{\"error\":%q,\"code\":%q}\n", test.message, test.code)
				if w.Code != test.status || w.Header().Get("Content-Type") != api.ContentTypeJSON || w.Body.String() != want {
					t.Fatalf("HTTP %d, headers %v, body %q; want HTTP %d, body %q", w.Code, w.Header(), w.Body.String(), test.status, want)
				}
			}
		})
	}
}

func TestLaneJSONFailureUsesPublicCode(t *testing.T) {
	s := New(newFakeRuntime())
	for _, test := range []struct {
		body    string
		message string
	}{
		{`{"unknown":"dummy-secret"}`, "invalid lane JSON body"},
		{`not-json`, "invalid lane JSON body"},
		{`{} {}`, "expected exactly one JSON object"},
		{`{} not-json`, "expected exactly one JSON object"},
	} {
		t.Run(test.body, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, api.PathLanes+"/shared/publish", strings.NewReader(test.body))
			var in api.LanePublish
			if s.readLaneJSON(w, r, &in) {
				t.Fatal("invalid lane JSON accepted")
			}
			want := fmt.Sprintf("{\"error\":%q,\"code\":%q}\n", test.message, api.CodeLaneInvalid)
			if w.Code != http.StatusBadRequest || w.Header().Get("Content-Type") != api.ContentTypeJSON || w.Body.String() != want {
				t.Fatalf("HTTP %d, headers %v, body %q; want HTTP 400, body %q", w.Code, w.Header(), w.Body.String(), want)
			}
		})
	}
}
