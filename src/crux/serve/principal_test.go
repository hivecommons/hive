package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/lanes"
)

const testProjectToken = "pppppppppppppppppppppppppppppppp"

// principalFixture is a token-bearing server with one lane project
// ("atlas") and a work-services route at /v1/whoami that echoes the caller.
func principalFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	cfg := lanes.Config{Principals: []lanes.Principal{{Project: "atlas", TokenEnv: "ATLAS"}}}
	auth, err := lanes.Bind(cfg, func(string) string { return testProjectToken }, "agent-admin")
	if err != nil {
		t.Fatal(err)
	}
	s := New(newFakeRuntime())
	s.Token, s.LaneAuth = "agent-admin", auth
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathHealth, s.handleHealth)
	mux.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		p, ok := CallerFrom(r.Context())
		if !ok {
			s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, "no caller")
			return
		}
		s.writeJSON(w, http.StatusOK, p)
	})
	mux.HandleFunc("GET /v1/whoami/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, _ := CallerFrom(r.Context())
		s.writeJSON(w, http.StatusOK, p)
	})
	s.workService("/v1/whoami")
	return s, s.authenticate(mux)
}

func callWith(h http.Handler, path, token string) (*httptest.ResponseRecorder, Principal, api.Error) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var p Principal
	var e api.Error
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
	} else {
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
	}
	return rec, p, e
}

func TestPrincipalControlTokenIsOperator(t *testing.T) {
	s, h := principalFixture(t)
	rec, p, _ := callWith(h, "/v1/whoami", "agent-admin")
	if rec.Code != http.StatusOK || p != (Principal{Kind: KindOperator, Name: DefaultOperatorName}) {
		t.Fatalf("control token: %d %+v", rec.Code, p)
	}
	if !p.IsOperator() || p.String() != "operator:operator" {
		t.Fatalf("operator predicate/string: %v %q", p.IsOperator(), p)
	}
	s.OperatorName = "hive-prod"
	if _, p, _ = callWith(h, "/v1/whoami", "agent-admin"); p.Name != "hive-prod" {
		t.Fatalf("configured operator name: %+v", p)
	}
	// The operator is also the caller on agent RPCs, which keep working.
	if rec, _, _ := callWith(h, api.PathHealth, "agent-admin"); rec.Code != http.StatusOK {
		t.Fatalf("health with control token: %d", rec.Code)
	}
}

func TestPrincipalProjectTokenOnlyOnWorkServices(t *testing.T) {
	_, h := principalFixture(t)
	for _, path := range []string{"/v1/whoami", "/v1/whoami/chk_1"} {
		rec, p, _ := callWith(h, path, testProjectToken)
		if rec.Code != http.StatusOK || p != (Principal{Kind: KindProject, Name: "atlas"}) {
			t.Fatalf("project token on %s: %d %+v %s", path, rec.Code, p, rec.Body)
		}
		if p.IsOperator() {
			t.Fatal("project principal must not be the operator")
		}
	}
	// A path that merely shares the prefix string is not a work service.
	if rec, _, e := callWith(h, "/v1/whoamix", testProjectToken); rec.Code != http.StatusUnauthorized || e.Code != api.CodeUnauthorized {
		t.Fatalf("prefix lookalike: %d %+v", rec.Code, e)
	}
	// Agent RPCs refuse project credentials exactly as before.
	rec, _, e := callWith(h, api.PathHealth, testProjectToken)
	if rec.Code != http.StatusUnauthorized || e.Code != api.CodeUnauthorized || !strings.Contains(e.Error, "project credentials cannot access agent RPCs") {
		t.Fatalf("project token on agent RPC: %d %+v", rec.Code, e)
	}
}

func TestPrincipalMissingOrWrongTokenUnchanged(t *testing.T) {
	_, h := principalFixture(t)
	for _, token := range []string{"", "wrong"} {
		for _, path := range []string{"/v1/whoami", api.PathHealth} {
			rec, _, e := callWith(h, path, token)
			if rec.Code != http.StatusUnauthorized || e.Code != api.CodeUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("token %q on %s: %d %+v", token, path, rec.Code, e)
			}
		}
	}
}

func TestPrincipalTokenlessUnixSocketIsOperator(t *testing.T) {
	s := New(newFakeRuntime())
	var got Principal
	var ok bool
	h := s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = CallerFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, api.PathHealth, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(context.WithValue(req.Context(), networkKey, networkUnix)))
	if rec.Code != http.StatusOK || !ok || got != s.operator() {
		t.Fatalf("token-less unix request: %d %v %+v", rec.Code, ok, got)
	}
	if _, ok := CallerFrom(context.Background()); ok {
		t.Fatal("a context outside the auth gate has no caller")
	}
}
