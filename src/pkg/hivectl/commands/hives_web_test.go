package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

func newCommonsWebHarness(t *testing.T, set *hivectl.ProfileSet) (*httptest.Server, *hivectl.ProfileStore, *atomic.Int64) {
	t.Helper()
	store := hivectl.NewProfileStore(t.TempDir())
	if set != nil {
		if err := store.Save(set); err != nil {
			t.Fatalf("seed profiles: %v", err)
		}
	}
	signals := &atomic.Int64{}
	deps := &hivesDeps{
		store: store,
		registrar: &fakeRegistrar{result: hubRegistration{
			RegistrationToken: "tok-new",
			ContributorID:     "contrib-new",
		}},
		githubUser: func(context.Context) (string, error) { return "octocat", nil },
		signalRelay: func(context.Context, *hivectl.ProfileStore) (hivectl.RelaySwitchResult, error) {
			signals.Add(1)
			return hivectl.RelaySwitchResult{Running: true, Target: "pid 123"}, nil
		},
		now: func() time.Time { return time.Date(2026, 9, 25, 5, 0, 0, 0, time.UTC) },
	}
	return httptest.NewServer(newCommonsWebServer(deps, "secret")), store, signals
}

func commonsReq(t *testing.T, server *httptest.Server, method, path string, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request: %v", err)
		}
	}
	req, err := http.NewRequest(method, server.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(commonsWebAuthHeader, "secret")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func commonsDo(t *testing.T, server *httptest.Server, method, path string, body any) (*http.Response, commonsWebListResponse) {
	t.Helper()
	resp, err := server.Client().Do(commonsReq(t, server, method, path, body))
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out commonsWebListResponse
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s status = %d", method, path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp, out
}

func TestCommonsWebRequiresTokenAndNeverExposesRegistrationTokens(t *testing.T) {
	server, _, _ := newCommonsWebHarness(t, twoHives())
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/api/hives")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized API status = %d, want 401", resp.StatusCode)
	}

	resp, err = server.Client().Get(server.URL + "/?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page bytes.Buffer
	if _, err := page.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page.String(), "tok-acme") || strings.Contains(page.String(), "tok-other") {
		t.Fatalf("web page exposed a registration token:\n%s", page.String())
	}

	_, list := commonsDo(t, server, http.MethodGet, "/api/hives", nil)
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "tok-acme") || strings.Contains(string(raw), "tok-other") {
		t.Fatalf("API exposed a registration token: %s", raw)
	}
	if list.Strategy != hivectl.CommonsStrategyRanked || len(list.Hives) != 2 || !list.Hives[0].Active {
		t.Fatalf("list response = %+v", list)
	}
}

func TestCommonsWebMutatesTheSharedProfileStore(t *testing.T) {
	server, store, signals := newCommonsWebHarness(t, twoHives())
	defer server.Close()

	_, list := commonsDo(t, server, http.MethodPost, "/api/hives", commonsSubscribeRequest{Name: "third", Hub: "wss://third.example/contribute"})
	if len(list.Hives) != 3 || signals.Load() != 1 {
		t.Fatalf("subscribe list/signals = %+v / %d", list, signals.Load())
	}
	set, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if third, _ := set.Find("third"); third == nil || third.RegistrationToken != "tok-new" {
		t.Fatalf("subscribed profile = %+v", third)
	}

	_, list = commonsDo(t, server, http.MethodPost, "/api/hives/third/move", commonsMoveRequest{Direction: "up"})
	if list.Hives[1].Name != "third" || signals.Load() != 2 {
		t.Fatalf("move list/signals = %+v / %d", list.Hives, signals.Load())
	}

	_, list = commonsDo(t, server, http.MethodPost, "/api/strategy", commonsStrategyRequest{Strategy: hivectl.CommonsStrategyNeediest})
	if list.Strategy != hivectl.CommonsStrategyNeediest || signals.Load() != 3 {
		t.Fatalf("strategy/signals = %s / %d", list.Strategy, signals.Load())
	}
	env, err := os.ReadFile(filepath.Join(filepath.Dir(store.Path()), "contributor.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "HIVE_COMMONS_STRATEGY=neediest") {
		t.Fatalf("projection missing strategy:\n%s", env)
	}

	_, list = commonsDo(t, server, http.MethodDelete, "/api/hives/third", nil)
	if len(list.Hives) != 2 || signals.Load() != 4 {
		t.Fatalf("delete list/signals = %+v / %d", list.Hives, signals.Load())
	}
}

func TestCommonsWebSubscribeDuplicateDoesNotRegister(t *testing.T) {
	store := hivectl.NewProfileStore(t.TempDir())
	if err := store.Save(twoHives()); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistrar{result: hubRegistration{RegistrationToken: "tok-new", ContributorID: "contrib-new"}}
	deps := &hivesDeps{
		store:      store,
		registrar:  reg,
		githubUser: func(context.Context) (string, error) { return "octocat", nil },
		now:        func() time.Time { return time.Now() },
	}
	server := httptest.NewServer(newCommonsWebServer(deps, "secret"))
	defer server.Close()

	resp, err := server.Client().Do(commonsReq(t, server, http.MethodPost, "/api/hives", commonsSubscribeRequest{Name: "acme", Hub: "wss://new.example/contribute"}))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate subscribe status = %d, want 400", resp.StatusCode)
	}
	if len(reg.calls) != 0 {
		t.Fatalf("duplicate subscribe registered remotely: %v", reg.calls)
	}
}

func TestCommonsWebMoveUsesDisplayedRankOrder(t *testing.T) {
	set := twoHives()
	set.Active = "other"
	server, _, _ := newCommonsWebHarness(t, set)
	defer server.Close()

	_, before := commonsDo(t, server, http.MethodGet, "/api/hives", nil)
	if before.Hives[0].Name != "other" {
		t.Fatalf("displayed rank = %+v, want other first", before.Hives)
	}
	_, after := commonsDo(t, server, http.MethodPost, "/api/hives/other/move", commonsMoveRequest{Direction: "down"})
	if after.Hives[0].Name != "acme" || after.Hives[1].Name != "other" {
		t.Fatalf("move used raw profile order, got %+v", after.Hives)
	}
}

func TestCommonsWebListenAddressMustBeLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:8080", "[::1]:0"} {
		if err := validateLoopbackListenAddr(addr); err != nil {
			t.Fatalf("validateLoopbackListenAddr(%q): %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:0", ":8080", "192.0.2.10:8080"} {
		if err := validateLoopbackListenAddr(addr); err == nil {
			t.Fatalf("validateLoopbackListenAddr(%q) = nil, want error", addr)
		}
	}
}

func TestRunHivesWebPrintsTokenizedLoopbackURLAndStopsOnContext(t *testing.T) {
	h := newHivesHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	env := &commandEnv{options: &rootOptions{timeout: time.Second}}

	if err := env.runHivesWeb(cmd, &hivesWebOptions{}); err != nil {
		t.Fatalf("runHivesWeb() error = %v", err)
	}
	printed := out.String()
	if !strings.Contains(printed, "The Commons web UI: http://127.0.0.1:") || !strings.Contains(printed, "/?token=") {
		t.Fatalf("printed URL = %q", printed)
	}
	if len(h.reg.calls) != 0 {
		t.Fatalf("starting web UI registered remotely: %v", h.reg.calls)
	}
}

func TestCommonsWebEmptyStoreAndQueryToken(t *testing.T) {
	server, _, _ := newCommonsWebHarness(t, nil)
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/api/hives?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query-token list status = %d", resp.StatusCode)
	}
	var list commonsWebListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Strategy != hivectl.CommonsStrategyRanked || len(list.Hives) != 0 {
		t.Fatalf("empty list = %+v", list)
	}
}

func TestCommonsWebReportsBadRequests(t *testing.T) {
	server, _, _ := newCommonsWebHarness(t, twoHives())
	defer server.Close()

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
		status int
	}{
		{name: "bad json", method: http.MethodPost, path: "/api/strategy", body: "{", status: http.StatusBadRequest},
		{name: "bad strategy", method: http.MethodPost, path: "/api/strategy", body: commonsStrategyRequest{Strategy: "random"}, status: http.StatusBadRequest},
		{name: "missing profile", method: http.MethodDelete, path: "/api/hives/missing", status: http.StatusNotFound},
		{name: "bad move direction", method: http.MethodPost, path: "/api/hives/acme/move", body: commonsMoveRequest{Direction: "sideways"}, status: http.StatusBadRequest},
		{name: "not found", method: http.MethodGet, path: "/api/nope", status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if raw, ok := tc.body.(string); ok {
				var err error
				req, err = http.NewRequest(tc.method, server.URL+tc.path, strings.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set(commonsWebAuthHeader, "secret")
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = commonsReq(t, server, tc.method, tc.path, tc.body)
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func TestCommonsWebPageRequiresExactToken(t *testing.T) {
	server, _, _ := newCommonsWebHarness(t, twoHives())
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("page with wrong token status = %d, want 401", resp.StatusCode)
	}
}

func TestCommonsWebSubscribeValidationAndRegistrationFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		req        commonsSubscribeRequest
		userErr    error
		reg        *fakeRegistrar
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "invalid name",
			req:        commonsSubscribeRequest{Name: "bad name", Hub: "wss://new.example/contribute"},
			reg:        &fakeRegistrar{result: hubRegistration{RegistrationToken: "tok"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid hub",
			req:        commonsSubscribeRequest{Name: "new", Hub: "ftp://new.example/contribute"},
			reg:        &fakeRegistrar{result: hubRegistration{RegistrationToken: "tok"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "github user fails",
			req:        commonsSubscribeRequest{Name: "new", Hub: "wss://new.example/contribute"},
			userErr:    errors.New("no user"),
			reg:        &fakeRegistrar{result: hubRegistration{RegistrationToken: "tok"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "remote registration fails",
			req:        commonsSubscribeRequest{Name: "new", Hub: "wss://new.example/contribute"},
			reg:        &fakeRegistrar{err: errors.New("hub down")},
			wantStatus: http.StatusBadRequest,
			wantCalls:  1,
		},
		{
			name:       "already registered remotely",
			req:        commonsSubscribeRequest{Name: "new", Hub: "wss://new.example/contribute"},
			reg:        &fakeRegistrar{result: hubRegistration{Message: "already registered"}},
			wantStatus: http.StatusBadRequest,
			wantCalls:  1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := hivectl.NewProfileStore(t.TempDir())
			userErr := tc.userErr
			deps := &hivesDeps{
				store:     store,
				registrar: tc.reg,
				githubUser: func(context.Context) (string, error) {
					if userErr != nil {
						return "", userErr
					}
					return "octocat", nil
				},
				now: func() time.Time { return time.Now() },
			}
			server := httptest.NewServer(newCommonsWebServer(deps, "secret"))
			defer server.Close()

			resp, err := server.Client().Do(commonsReq(t, server, http.MethodPost, "/api/hives", tc.req))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := len(tc.reg.calls); got != tc.wantCalls {
				t.Fatalf("remote registrations = %d, want %d (%v)", got, tc.wantCalls, tc.reg.calls)
			}
		})
	}
}

func TestCommonsWebSignalErrorDoesNotHideSavedChange(t *testing.T) {
	store := hivectl.NewProfileStore(t.TempDir())
	deps := &hivesDeps{
		store: store,
		registrar: &fakeRegistrar{result: hubRegistration{
			RegistrationToken: "tok-new",
			ContributorID:     "contrib-new",
		}},
		githubUser: func(context.Context) (string, error) { return "octocat", nil },
		signalRelay: func(context.Context, *hivectl.ProfileStore) (hivectl.RelaySwitchResult, error) {
			return hivectl.RelaySwitchResult{}, errors.New("no relay")
		},
		now: func() time.Time { return time.Now() },
	}
	server := httptest.NewServer(newCommonsWebServer(deps, "secret"))
	defer server.Close()

	resp, err := server.Client().Do(commonsReq(t, server, http.MethodPost, "/api/hives", commonsSubscribeRequest{Name: "new", Hub: "wss://new.example/contribute"}))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	set, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if profile, _ := set.Find("new"); profile == nil {
		t.Fatal("profile was not saved before relay signal error")
	}
}

func TestNewHivesWebCommandDefaults(t *testing.T) {
	cmd := newHivesWebCommand(&commandEnv{})
	if cmd.Use != "web" {
		t.Fatalf("Use = %q", cmd.Use)
	}
	flag := cmd.Flags().Lookup("addr")
	if flag == nil || flag.DefValue != defaultCommonsWebAddr {
		t.Fatalf("addr flag = %#v", flag)
	}
}
