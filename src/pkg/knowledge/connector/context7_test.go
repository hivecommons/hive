package connector

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func c7Cfg(scope map[string]string) ConnectorConfig {
	return ConnectorConfig{Name: "ctx7", Type: TypeContext7, Enabled: true, Layer: "community", Scope: scope}
}

func TestContext7ConnectorValidate(t *testing.T) {
	tests := []struct {
		name    string
		scope   map[string]string
		wantErr string
	}{
		{"ok", map[string]string{"library_ids": "/vercel/next.js, /golang/go"}, ""},
		{"missing", map[string]string{}, "scope.library_ids is required"},
		{"no slash", map[string]string{"library_ids": "@evil.com/x"}, "must look like /org/project"},
		{"dup", map[string]string{"library_ids": "/a/b,/a/b"}, "listed twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DefaultRegistry().New(c7Cfg(tt.scope), Deps{})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func stubContext7(t *testing.T, fail string) *[]string {
	t.Helper()
	orig := fetchContext7Docs
	t.Cleanup(func() { fetchContext7Docs = orig })
	var keys []string
	fetchContext7Docs = func(_ context.Context, id, topic, key string) (*knowledge.Context7DocsResult, error) {
		keys = append(keys, key)
		if id == fail {
			return nil, errors.New("HTTP 500 from Context7")
		}
		return &knowledge.Context7DocsResult{LibraryID: id, Title: id, Content: "docs " + id + " " + topic}, nil
	}
	return &keys
}

func TestContext7ConnectorSync(t *testing.T) {
	keys := stubContext7(t, "")
	t.Setenv(Context7APIKeyEnv, "env-key")
	cfg := c7Cfg(map[string]string{"library_ids": "/a/one,/b/two", "topic": "routing"})
	c, err := DefaultRegistry().New(cfg, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if fl, ok := c.(FullLister); !ok || !fl.FullListing() {
		t.Fatal("context7 connector must be a full lister")
	}
	var pages []Page
	cur, err := c.Sync(context.Background(), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil || len(cur) != 16 {
		t.Fatalf("Sync = %q, %v", cur, err)
	}
	if len(pages) != 2 || pages[0].ID != "/a/one" || pages[0].URL != "context7:///a/one" || pages[1].Markdown != "docs /b/two routing" {
		t.Fatalf("pages = %+v", pages)
	}
	if (*keys)[0] != "env-key" {
		t.Fatalf("fallback env key not used: %v", *keys)
	}
}

func TestContext7ConnectorAuthAndErrors(t *testing.T) {
	keys := stubContext7(t, "/b/two")
	t.Setenv("HIVE_TEST_C7_KEY", "ref-key")
	cfg := c7Cfg(map[string]string{"library_ids": "/a/one"})
	cfg.Auth = Auth{Env: "HIVE_TEST_C7_KEY"}
	c, _ := DefaultRegistry().New(cfg, Deps{})
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if (*keys)[0] != "ref-key" {
		t.Fatalf("referenced key not used: %v", *keys)
	}

	cfg.Auth = Auth{Env: "HIVE_TEST_C7_UNSET"}
	c, _ = DefaultRegistry().New(cfg, Deps{})
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); err == nil {
		t.Fatal("expected unset auth.env error")
	}

	cfg = c7Cfg(map[string]string{"library_ids": "/b/two"})
	c, _ = DefaultRegistry().New(cfg, Deps{})
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); err == nil || !strings.Contains(err.Error(), "library /b/two") {
		t.Fatalf("err = %v", err)
	}
}
