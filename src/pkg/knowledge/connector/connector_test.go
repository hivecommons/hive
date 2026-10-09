package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAuthSecret(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "tok")
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(good, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_CONNECTOR_TEST_TOKEN", " envtok ")
	t.Setenv("HIVE_CONNECTOR_TEST_EMPTY", "")

	tests := []struct {
		name    string
		auth    Auth
		want    string
		wantErr string
	}{
		{"env", Auth{Env: "HIVE_CONNECTOR_TEST_TOKEN"}, "envtok", ""},
		{"env empty", Auth{Env: "HIVE_CONNECTOR_TEST_EMPTY"}, "", "empty or unset"},
		{"file", Auth{File: good}, "s3cret", ""},
		{"file empty", Auth{File: empty}, "", "is empty"},
		{"file missing", Auth{File: filepath.Join(dir, "nope")}, "", "reading auth.file"},
		{"both", Auth{Env: "X", File: good}, "", "mutually exclusive"},
		{"none", Auth{}, "", "no credential configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.auth.Secret()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Secret() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if !errors.Is(func() error { _, err := (Auth{}).Secret(); return err }(), ErrNoCredential) {
		t.Fatal("empty auth should return ErrNoCredential")
	}
	if (Auth{}).Configured() || !(Auth{Env: "X"}).Configured() || !(Auth{File: "/x"}).Configured() {
		t.Fatal("Configured() wrong")
	}
}

func TestConnectorConfigHelpers(t *testing.T) {
	c := ConnectorConfig{Scope: map[string]string{"url": "  https://x  "}}
	if c.ScopeValue("url") != "https://x" || c.ScopeValue("missing") != "" {
		t.Fatalf("ScopeValue wrong")
	}
	if c.EffectiveInterval() != DefaultInterval {
		t.Fatalf("default interval = %v", c.EffectiveInterval())
	}
	c.Interval = time.Hour
	if c.EffectiveInterval() != time.Hour {
		t.Fatalf("interval = %v", c.EffectiveInterval())
	}
}

func TestRegistryRegister(t *testing.T) {
	r := NewRegistry()
	f := func(ConnectorConfig, Deps) (Connector, error) { return &fakeConn{typ: "fake"}, nil }
	if err := r.Register("Bad Type", f); err == nil {
		t.Fatal("invalid type name accepted")
	}
	if err := r.Register("fake", nil); err == nil {
		t.Fatal("nil factory accepted")
	}
	if err := r.Register("fake", f); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("fake", f); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate err = %v", err)
	}
	if err := r.Register("another", f); err != nil {
		t.Fatal(err)
	}
	if got := r.Types(); !reflect.DeepEqual(got, []string{"another", "fake"}) {
		t.Fatalf("Types() = %v", got)
	}
	if got := DefaultRegistry().Types(); !reflect.DeepEqual(got, []string{TypeDocument, TypeGit, TypeGitHubWiki, TypeGoogleDrive, TypeRepoWiki, TypeSharePoint}) {
		t.Fatalf("DefaultRegistry().Types() = %v", got)
	}
}

func TestRegistryNew(t *testing.T) {
	r := NewRegistry()
	_ = r.Register("fake", func(cfg ConnectorConfig, _ Deps) (Connector, error) {
		switch cfg.Scope["mode"] {
		case "factory-error":
			return nil, errors.New("boom")
		case "wrong-type":
			return &fakeConn{typ: "other"}, nil
		case "invalid":
			return &fakeConn{typ: "fake", validateErr: errors.New("scope.x required")}, nil
		}
		return &fakeConn{typ: "fake"}, nil
	})
	base := ConnectorConfig{Name: "n1", Type: "fake", Layer: "project"}
	tests := []struct {
		name    string
		mutate  func(*ConnectorConfig)
		wantErr string
	}{
		{"ok", func(*ConnectorConfig) {}, ""},
		{"bad name", func(c *ConnectorConfig) { c.Name = "N 1" }, "connector name"},
		{"bad type", func(c *ConnectorConfig) { c.Type = "" }, "type \"\""},
		{"bad layer", func(c *ConnectorConfig) { c.Layer = "team" }, "layer \"team\""},
		{"both auth", func(c *ConnectorConfig) { c.Auth = Auth{Env: "A", File: "/b"} }, "mutually exclusive"},
		{"unknown type", func(c *ConnectorConfig) { c.Type = "notion" }, "unknown type \"notion\" (known: fake)"},
		{"factory error", func(c *ConnectorConfig) { c.Scope = map[string]string{"mode": "factory-error"} }, "connector n1: boom"},
		{"wrong type", func(c *ConnectorConfig) { c.Scope = map[string]string{"mode": "wrong-type"} }, "returned type \"other\""},
		{"invalid", func(c *ConnectorConfig) { c.Scope = map[string]string{"mode": "invalid"} }, "connector n1: scope.x required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			c, err := r.New(cfg, Deps{})
			if tt.wantErr == "" {
				if err != nil || c == nil {
					t.Fatalf("New() = %v, %v", c, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestIsFullListing(t *testing.T) {
	inc := &fakeConn{typ: "fake"}
	full := &fullFakeConn{fakeConn: &fakeConn{typ: "fake"}}
	if !isFullListing(inc, "") {
		t.Fatal("empty cursor must be a full listing")
	}
	if isFullListing(inc, "c1") {
		t.Fatal("incremental connector with cursor is not a full listing")
	}
	if !isFullListing(full, "c1") {
		t.Fatal("FullLister must be a full listing")
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Fatalf("zero sleep: %v", err)
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("short sleep: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sleep = %v", err)
	}
	if err := sleepCtx(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled zero sleep = %v", err)
	}
}

func TestHTTPStatusRetryable(t *testing.T) {
	for code, want := range map[int]bool{429: true, 503: true, 502: true, 504: true, 500: false, 404: false, 200: false} {
		if got := httpStatusRetryable(code); got != want {
			t.Errorf("httpStatusRetryable(%d) = %v", code, got)
		}
	}
}

func TestDepsDefaults(t *testing.T) {
	var d Deps
	if d.logger() == nil || d.httpClient() == nil {
		t.Fatal("zero Deps must supply defaults")
	}
	h := NewHTTPClient(HTTPOptions{})
	d.HTTP = h
	if d.httpClient() != h {
		t.Fatal("explicit HTTP client ignored")
	}
}
