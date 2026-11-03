package config

import "strings"

const (
	// DefaultPprofAddr is where the net/http/pprof listener binds when
	// debug.pprof_addr is unset. Loopback only: the profiles leak source paths
	// and heap contents, so they are reached via `kubectl port-forward` or an
	// in-container curl, never over the pod network.
	DefaultPprofAddr = "127.0.0.1:6060"
	// PprofAddrOff is the debug.pprof_addr sentinel that disables the listener.
	PprofAddrOff = "off"
)

// DebugConfig is the optional `debug:` block for process-level diagnostics
// (hivecommons/hive#14681).
type DebugConfig struct {
	// PprofAddr is the host:port the net/http/pprof listener binds to. Empty
	// means DefaultPprofAddr; "off" disables it. The host must be a loopback
	// address — any other host is rejected at startup and falls back to the
	// default with a logged warning, so a typo can never expose profiles.
	PprofAddr string `yaml:"pprof_addr,omitempty" json:"pprof_addr,omitempty"`
}

// EffectivePprofAddr resolves PprofAddr to the address to bind, or "" when
// the listener is disabled.
func (d DebugConfig) EffectivePprofAddr() string {
	addr := strings.TrimSpace(d.PprofAddr)
	switch {
	case addr == "":
		return DefaultPprofAddr
	case strings.EqualFold(addr, PprofAddrOff):
		return ""
	}
	return addr
}
