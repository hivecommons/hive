package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestServeConfigValidateDefaultsAndFailsClosed(t *testing.T) {
	t.Setenv(ServeSocketEnvVar, "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	var c ServeConfig
	if err := c.Validate(); err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if c.Socket != DefaultServeSocket() {
		t.Fatalf("Socket = %q, want default %q", c.Socket, DefaultServeSocket())
	}

	c = ServeConfig{Listen: "127.0.0.1:9000"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), ServeTokenEnvVar) {
		t.Fatalf("TCP without token must be refused, got %v", err)
	}

	c = ServeConfig{Socket: "/x/y.sock", Listen: "127.0.0.1:9000", Token: "s3cret"}
	if err := c.Validate(); err != nil || c.Socket != "/x/y.sock" {
		t.Fatalf("explicit socket + token: err %v socket %q", err, c.Socket)
	}
}

func TestDefaultServeSocketPrecedence(t *testing.T) {
	t.Setenv(ServeSocketEnvVar, "/override/crux.sock")
	if got := DefaultServeSocket(); got != "/override/crux.sock" {
		t.Fatalf("%s should win, got %q", ServeSocketEnvVar, got)
	}

	t.Setenv(ServeSocketEnvVar, "")
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if got, want := DefaultServeSocket(), filepath.Join(runtimeDir, ServeSocketDirName, ServeSocketName); got != want {
		t.Fatalf("XDG default = %q, want %q", got, want)
	}

	t.Setenv("XDG_RUNTIME_DIR", "")
	got := DefaultServeSocket()
	if !strings.HasPrefix(filepath.Base(filepath.Dir(got)), ServeSocketDirName+"-") || filepath.Base(got) != ServeSocketName {
		t.Fatalf("temp-dir default = %q, want <tmp>/%s-<uid>/%s", got, ServeSocketDirName, ServeSocketName)
	}
}
