package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

func TestCustomersHubMatcherPostsHashesWithBearer(t *testing.T) {
	t.Setenv(spoke.EnvHeartbeatKey, "bearer-1")
	var got struct {
		HiveID string   `json:"hive_id"`
		Hashes []string `json:"hashes"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != customersHubMatchPath {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer bearer-1" {
			t.Errorf("bearer = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("no cookie may be sent")
		}
		if r.Header.Get(customers.HubMatchLabelsHeader) != customers.HubMatchLabelsV1 {
			t.Errorf("labels header = %q, want the public-fields labels request", r.Header.Get(customers.HubMatchLabelsHeader))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		_, _ = w.Write([]byte(`{"matches":[{"match_hash":"h1","real_name":"Ada\u0007 L","notes":"never read","cleared":["organization","tags"]}]}`))
	}))
	defer srv.Close()

	reply, err := customersHubMatcher{hubURL: srv.URL + "/", hiveID: "hive-a"}.Match(context.Background(), []string{"h1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.HiveID != "hive-a" || len(got.Hashes) != 1 || got.Hashes[0] != "h1" {
		t.Fatalf("request body = %+v", got)
	}
	if len(reply.Matches) != 1 {
		t.Fatalf("matches = %+v", reply.Matches)
	}
	m := reply.Matches[0]
	if m.RealName != "Ada L" || len(m.Cleared) != 1 || m.Cleared[0] != customers.FieldOrganization {
		t.Fatalf("reply not sanitised: %+v", m)
	}
}

func TestCustomersHubMatcherRefusal(t *testing.T) {
	t.Setenv(spoke.EnvHeartbeatKey, "bearer-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	if _, err := (customersHubMatcher{hubURL: srv.URL, hiveID: "hive-a"}).Match(context.Background(), []string{"h1"}); err == nil {
		t.Fatal("a 404 (hub sharing off) must be an error")
	}
}

func TestCustomersHubImportOffMakesNoRequest(t *testing.T) {
	t.Setenv(config.EnvCustomersEnabled, "")
	t.Setenv(config.EnvCustomersHubImportEnabled, "")
	t.Setenv(spoke.EnvHeartbeatKey, "bearer-1")
	t.Setenv(spoke.EnvCustomerMatchKey, "match-key")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"matches":[]}`))
	}))
	defer srv.Close()

	store, err := customers.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}}, "owner"); err != nil {
		t.Fatal(err)
	}
	on := true
	cases := map[string]*config.Config{
		"switch off": {HiveID: "hive-a", Hub: config.HubConfig{Enabled: true, URL: srv.URL}},
		"no hub":     {HiveID: "hive-a", Customers: config.CustomersConfig{HubImport: config.CustomersHubImportConfig{Enabled: &on}}},
	}
	for name, cfg := range cases {
		res := newCustomersHubImport(store, cfg, nil).Run(context.Background(), "")
		if res.Requests != 0 || res.Skipped != customers.HubSkipDisabled {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("hub was called %d times with the import off", hits.Load())
	}

	cfg := &config.Config{HiveID: "hive-a", Hub: config.HubConfig{Enabled: true, URL: srv.URL},
		Customers: config.CustomersConfig{HubImport: config.CustomersHubImportConfig{Enabled: &on}}}
	if res := newCustomersHubImport(store, cfg, nil).Run(context.Background(), ""); res.Requests != 1 {
		t.Fatalf("on: %+v", res)
	}
	if hits.Load() != 1 {
		t.Fatalf("hub hits = %d, want 1", hits.Load())
	}
}
