package main

import (
	"fmt"
	"log/slog"

	"github.com/hivecommons/hive/crux"
	cruxconfig "github.com/hivecommons/hive/crux/config"
	cruxruntime "github.com/hivecommons/hive/crux/runtime"
)

// hostedRuntime is the runtime as the boot sequence sees it: the crux.Runtime
// families plus crux.Host, the in-process wiring Hive installs (credential
// minters, launch-time resolvers, observers). Only the construction site and
// the installers that call Set* need it; everything else takes crux.Runtime
// or a narrower family. In sidecar mode (#14138 stage 3) the Host half is
// sidecarHost: observers fed from the runtime's event stream, capabilities
// served back over the hive callback endpoint.
type hostedRuntime interface {
	crux.Runtime
	crux.Host
}

// newHostedRuntime builds the runtime selected by CRUX_MODE (in-process by
// default) and returns it with the Host wiring the boot sequence relies on:
// the in-process manager is a Host itself; the sidecar client is wrapped in
// sidecarHost. Anything else is a boot error, not a silent downgrade.
func newHostedRuntime(cfg cruxconfig.RuntimeConfig, opts ...cruxruntime.Option) (hostedRuntime, error) {
	rt, err := cruxruntime.New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return hostRuntime(cfg, rt, nil)
}

func hostRuntime(cfg cruxconfig.RuntimeConfig, rt crux.Runtime, logger *slog.Logger) (hostedRuntime, error) {
	if hr, ok := rt.(hostedRuntime); ok {
		return hr, nil
	}
	if cfg.Mode == cruxconfig.ModeSidecar {
		return newSidecarHost(rt, logger), nil
	}
	return nil, fmt.Errorf("crux runtime mode %q (%T): %w", cfg.Mode, rt, cruxruntime.ErrNotInProcess)
}
