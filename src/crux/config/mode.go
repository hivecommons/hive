package config

import (
	"fmt"
	"os"
	"strings"
)

// How a host reaches the runtime (hivecommons/hive#14138, sidecar plan stage
// 1). Like ServeConfig this is a process-level setting, not a hive.yaml key:
// it is chosen by whoever composes the process (cmd/hive, a harness), so it
// is not reachable from Config and the settings-parity walk never sees it.
// The hive.yaml key lands with the supervision story (stage 4), which is
// when an operator first has something to configure about the sidecar.

// Mode selects the Runtime implementation crux/runtime.New returns.
type Mode string

const (
	// ModeInProcess links crux/agent into the host and constructs the
	// manager directly. The default.
	ModeInProcess Mode = "inprocess"
	// ModeSidecar drives a `crux serve` process at RuntimeConfig.Server over
	// the Crux API through crux/api/client (stage 3 of hivecommons/hive#14138).
	ModeSidecar Mode = "sidecar"

	// ModeEnvVar names the environment variable RuntimeConfigFromEnv reads
	// the mode from. Empty or unset means ModeInProcess.
	ModeEnvVar = "CRUX_MODE"
)

// DefaultMode is the mode an empty RuntimeConfig resolves to.
const DefaultMode = ModeInProcess

// Modes lists the recognised modes, in documentation order.
func Modes() []Mode { return []Mode{ModeInProcess, ModeSidecar} }

// ParseMode normalises s (case-insensitive, surrounding space ignored) to a
// Mode. Empty is DefaultMode; anything else unrecognised is an error naming
// the accepted values.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return DefaultMode, nil
	case ModeInProcess, ModeSidecar:
		return m, nil
	default:
		return "", fmt.Errorf("unknown crux runtime mode %q (want %s)", s, modeList())
	}
}

func modeList() string {
	names := make([]string, 0, len(Modes()))
	for _, m := range Modes() {
		names = append(names, string(m))
	}
	return strings.Join(names, " or ")
}

// RuntimeConfig is what crux/runtime.New needs to pick and reach a runtime.
// The zero value is the in-process default.
type RuntimeConfig struct {
	// Mode selects the implementation. Empty means DefaultMode.
	Mode Mode
	// Server is the `crux serve` address for ModeSidecar, in the forms
	// api.ParseAddress accepts. Ignored in-process. Empty means the default
	// socket (DefaultServeSocket).
	Server string
	// Token is the bearer token for Server. Ignored in-process.
	Token string
}

// Validate applies the defaults and rejects an unknown mode. It is the one
// place the mode string is interpreted, so a typo fails at construction
// rather than silently running in-process.
func (c *RuntimeConfig) Validate() error {
	m, err := ParseMode(string(c.Mode))
	if err != nil {
		return err
	}
	c.Mode = m
	if c.Mode == ModeSidecar && c.Server == "" {
		c.Server = DefaultServeSocket()
	}
	return nil
}

// RuntimeConfigFromEnv reads ModeEnvVar, ServerEnvVar and ServeTokenEnvVar.
// It does not validate; callers pass the result to crux/runtime.New, which
// does.
func RuntimeConfigFromEnv() RuntimeConfig {
	return RuntimeConfig{
		Mode:   Mode(os.Getenv(ModeEnvVar)),
		Server: os.Getenv(ServerEnvVar),
		Token:  os.Getenv(ServeTokenEnvVar),
	}
}
