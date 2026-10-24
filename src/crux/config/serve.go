package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Where `crux serve` listens and how a client finds it (Crux Stage 3,
// hivecommons/hive#13536; src/docs/design/crux.md "Stage 3"). These are
// process-level settings, not hive.yaml keys: the server is started by an
// operator or a harness, and the client is pointed at it by flag or
// environment. Nothing here is reachable from Config, so the settings-parity
// walk never sees it.
const (
	// ServeSocketEnvVar overrides the default unix socket path for
	// `crux serve` and for a client that is given no --server.
	ServeSocketEnvVar = "CRUX_SOCKET"
	// ServerEnvVar is the address a `crux` client talks to instead of building
	// an in-process runtime: `unix:///path`, an absolute socket path, or
	// `host:port` / `http://host:port` for a TCP listener.
	ServerEnvVar = "CRUX_SERVER"
	// ServeTokenEnvVar is the bearer token the server requires and the client
	// presents. A TCP listener refuses to start without one.
	ServeTokenEnvVar = "CRUX_TOKEN"
	// ServeSocketName is the socket file name under ServeSocketDir.
	ServeSocketName = "crux.sock"
	// ServeSocketDirName is the per-user directory under $XDG_RUNTIME_DIR or
	// the system temp dir that holds the socket.
	ServeSocketDirName = "crux"
	// ServeSocketMode is the permission the server sets on its socket file:
	// owner only, since a unix socket with no token is authenticated by the
	// file system alone.
	ServeSocketMode os.FileMode = 0o600
	// ServeSocketDirMode is the permission for a socket directory the server
	// creates.
	ServeSocketDirMode os.FileMode = 0o700
)

// ServeConfig is what `crux serve` listens on. Zero values mean "unix socket
// at DefaultServeSocket, no TCP".
type ServeConfig struct {
	// Socket is the unix socket path. Empty selects DefaultServeSocket().
	Socket string
	// Listen is an optional TCP host:port. It is refused without a Token.
	Listen string
	// Token is the bearer token every request must carry. Optional on the
	// socket (the file mode is the credential), required for Listen.
	Token string
}

// Validate applies the defaults and the fail-closed rule: no TCP without a
// token.
func (c *ServeConfig) Validate() error {
	if c.Socket == "" {
		c.Socket = DefaultServeSocket()
	}
	if c.Listen != "" && c.Token == "" {
		return fmt.Errorf("a TCP listener on %s needs a bearer token (--token or %s); refusing to serve the runtime unauthenticated", c.Listen, ServeTokenEnvVar)
	}
	return nil
}

// DefaultServeSocket is the socket path `crux serve` and the client agree on
// when neither is told otherwise: $CRUX_SOCKET if set, else
// $XDG_RUNTIME_DIR/crux/crux.sock, else <tmp>/crux-<uid>/crux.sock. The uid
// in the temp-dir fallback keeps two users on one host from colliding on a
// shared /tmp.
func DefaultServeSocket() string {
	if p := os.Getenv(ServeSocketEnvVar); p != "" {
		return p
	}
	return filepath.Join(DefaultServeSocketDir(), ServeSocketName)
}

// DefaultServeSocketDir is the directory DefaultServeSocket places the socket
// in; see there for the rule.
func DefaultServeSocketDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, ServeSocketDirName)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", ServeSocketDirName, os.Getuid()))
}
