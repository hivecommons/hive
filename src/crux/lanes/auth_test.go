package lanes

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
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
	if _, err := LoadConfig("testdata/missing-config.json"); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing config", err)
	}
	loaded, err := LoadConfig("testdata/config.json")
	if err != nil || !reflect.DeepEqual(loaded, testConfig("nats://127.0.0.1:4222")) {
		t.Fatalf("load: %+v %v", loaded, err)
	}
	if _, err := LoadConfig("testdata/unknown-config.json"); !errors.Is(err, ErrInvalid) {
		t.Fatal("unknown config field accepted", err)
	}
}

func TestConfigReaderStrictAndBounded(t *testing.T) {
	data, err := os.ReadFile("testdata/config.json")
	if err != nil {
		t.Fatal(err)
	}
	atLimit := string(data) + strings.Repeat(" ", maxConfigBytes-len(data))
	loaded, err := loadConfig(strings.NewReader(atLimit))
	if err != nil || !reflect.DeepEqual(loaded, testConfig("nats://127.0.0.1:4222")) {
		t.Fatalf("exact byte limit: %+v %v", loaded, err)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{"unknown field", `{"unknown":true}`},
		{"unknown nested field", `{"principals":[{"project":"atlas","token_env":"ATLAS_TOKEN","token":"dummy"}]}`},
		{"trailing object", string(data) + `{}`},
		{"trailing invalid JSON", string(data) + `not-json`},
		{"malformed JSON", `not-json`},
		{"empty input", ""},
		{"invalid configuration", `{}`},
		{"oversized whitespace", strings.Repeat(" ", maxConfigBytes+1)},
		{"oversized valid prefix", atLimit + " "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadConfig(strings.NewReader(test.body)); !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid config accepted", err)
			}
		})
	}
	if _, err := loadConfig(iotest.ErrReader(errors.New("dummy-read-detail"))); !errors.Is(err, ErrInvalid) {
		t.Fatal("failed read accepted", err)
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
