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

func TestServeConfigValidateRequiresLoopback(t *testing.T) {
	for _, listen := range []string{
		"localhost:7777", "LOCALHOST:7777", "127.0.0.1:7777", "127.0.0.2:7777",
		"127.255.255.255:7777", "[::1]:7777", "[0:0:0:0:0:0:0:1]:7777",
		"[::ffff:127.0.0.1]:7777",
	} {
		t.Run(listen, func(t *testing.T) {
			c := ServeConfig{Listen: listen, Token: "test-token"}
			if err := c.Validate(); err != nil {
				t.Fatalf("loopback listener refused: %v", err)
			}
			c.Token = ""
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), ServeTokenEnvVar) {
				t.Fatalf("loopback TCP must still require a bearer token: %v", err)
			}
		})
	}
	for _, listen := range []string{
		":7777", "0.0.0.0:7777", "[::]:7777", "10.0.0.5:7777", "192.0.2.1:7777",
		"[2001:db8::1]:7777", "[::ffff:10.0.0.5]:7777", "crux.example:7777",
		"localhost.example:7777", "localhost.:7777", "127.0.0.1.example:7777",
	} {
		t.Run(listen, func(t *testing.T) {
			c := ServeConfig{Listen: listen, Token: "test-token"}
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), "TLS-terminating reverse proxy on loopback") {
				t.Fatalf("non-loopback listener must be refused with TLS proxy guidance: %v", err)
			}
		})
	}
	for _, listen := range []string{"localhost", "127.0.0.1", "::1:7777", "[::1:7777"} {
		t.Run(listen, func(t *testing.T) {
			c := ServeConfig{Listen: listen, Token: "test-token"}
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "host:port") {
				t.Fatalf("malformed listener must be refused: %v", err)
			}
		})
	}
}
