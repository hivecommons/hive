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

// Supervision says who keeps the sidecar's `crux serve` process alive
// (hivecommons/hive#14138 stage 4).
type Supervision string

const (
	// SuperviseChild: the host spawns `crux serve` itself, waits for its
	// health route, and restarts it with backoff when it exits. The
	// default for ModeSidecar.
	SuperviseChild Supervision = "child"
	// SuperviseExternal: something else (systemd, a pod sidecar container)
	// runs `crux serve`; the host only reconnects.
	SuperviseExternal Supervision = "external"

	// SuperviseEnvVar selects the Supervision; empty means SuperviseChild.
	SuperviseEnvVar = "CRUX_SUPERVISE"
	// BinaryEnvVar names the `crux` executable SuperviseChild spawns;
	// empty means DefaultBinary looked up on PATH.
	BinaryEnvVar = "CRUX_BINARY"
	// DefaultBinary is the executable name SuperviseChild looks up.
	DefaultBinary = "crux"
)

// ParseSupervision normalises s to a Supervision; empty is SuperviseChild.
func ParseSupervision(s string) (Supervision, error) {
	switch v := Supervision(strings.ToLower(strings.TrimSpace(s))); v {
	case "":
		return SuperviseChild, nil
	case SuperviseChild, SuperviseExternal:
		return v, nil
	default:
		return "", fmt.Errorf("unknown crux supervision %q (want %s or %s)", s, SuperviseChild, SuperviseExternal)
	}
}

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
	// Supervise says whether the host spawns `crux serve` (SuperviseChild,
	// the default) or expects it run externally. Ignored in-process.
	Supervise Supervision
	// Binary is the `crux` executable SuperviseChild spawns; empty means
	// DefaultBinary on PATH. Ignored in-process and under SuperviseExternal.
	Binary string
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
	sv, err := ParseSupervision(string(c.Supervise))
	if err != nil {
		return err
	}
	c.Supervise = sv
	if c.Binary == "" {
		c.Binary = DefaultBinary
	}
	return nil
}

// RuntimeConfigFromEnv reads ModeEnvVar, ServerEnvVar, ServeTokenEnvVar,
// SuperviseEnvVar and BinaryEnvVar. It does not validate; callers pass the
// result to crux/runtime.New, which does.
func RuntimeConfigFromEnv() RuntimeConfig {
	return RuntimeConfig{
		Mode:      Mode(os.Getenv(ModeEnvVar)),
		Server:    os.Getenv(ServerEnvVar),
		Token:     os.Getenv(ServeTokenEnvVar),
		Supervise: Supervision(os.Getenv(SuperviseEnvVar)),
		Binary:    os.Getenv(BinaryEnvVar),
	}
}
