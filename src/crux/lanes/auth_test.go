package lanes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPrincipalBindingsAreFailClosed(t *testing.T) {
	cfg := testConfig("nats://127.0.0.1:4222")
	secrets := map[string]string{"ATLAS_TOKEN": strings.Repeat("a", minTokenBytes), "BEACON_TOKEN": strings.Repeat("b", minTokenBytes)}
	getenv := func(env string) string { return secrets[env] }
	auth, err := Bind(cfg, getenv, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := auth.Project(secrets["ATLAS_TOKEN"]); !ok || p != "atlas" {
		t.Fatal("wrong bound principal")
	}
	for _, token := range []string{"", "admin", "atlas", strings.Repeat("a", minTokenBytes-1)} {
		if _, ok := auth.Project(token); ok {
			t.Fatal("forged token accepted")
		}
	}
	var absent *Authenticator
	if _, ok := absent.Project("any"); ok {
		t.Fatal("nil authenticator accepted")
	}
	if _, err := Bind(cfg, getenv, secrets["ATLAS_TOKEN"]); !errors.Is(err, ErrInvalid) {
		t.Fatal("admin-token overlap accepted", err)
	}
	secrets["BEACON_TOKEN"] = secrets["ATLAS_TOKEN"]
	if _, err := Bind(cfg, getenv, "admin"); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate token accepted", err)
	}
	delete(secrets, "BEACON_TOKEN")
	if _, err := Bind(cfg, getenv, "admin"); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing token accepted", err)
	}
}

func TestLoadConfigStrictAndBounded(t *testing.T) {
	path := fmt.Sprintf(".lane-config-%d.json", time.Now().UnixNano())
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
	})
	if _, err := LoadConfig(path); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing config", err)
	}
	cfg := testConfig("nats://127.0.0.1:4222")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded.Namespace != DefaultNamespace {
		t.Fatalf("load: %+v %v", loaded, err)
	}
	for _, bad := range []string{`{"unknown":true}`, `{} {}`, `not-json`, strings.Repeat(" ", maxConfigBytes+1)} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid config accepted", err)
		}
	}
}

func TestRegistryAndConfigurationAccess(t *testing.T) {
	b := testBroker(t, nil)
	if b.Config().Namespace != DefaultNamespace {
		t.Fatal("wrong configuration")
	}
	for _, project := range []string{"atlas", "beacon"} {
		if len(b.Registry(project)) != 1 {
			t.Fatal("member cannot see lane")
		}
	}
	if len(b.Registry("outsider")) != 0 {
		t.Fatal("outsider can see lane")
	}
	if _, err := b.authorize("atlas", "shared", "admin", ""); !errors.Is(err, ErrForbidden) {
		t.Fatal("implicit ownership admin capability", err)
	}
	if _, err := b.authorize("atlas", "unknown", "publish", ""); !errors.Is(err, ErrForbidden) {
		t.Fatal("unknown lane", err)
	}
	if _, err := b.Replay(context.Background(), "atlas", "shared", 0, 0); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid replay batch", err)
	}
}
