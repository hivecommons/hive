package main

import (
	"fmt"

	"github.com/hivecommons/hive/crux"
	cruxconfig "github.com/hivecommons/hive/crux/config"
	cruxruntime "github.com/hivecommons/hive/crux/runtime"
)

// hostedRuntime is the runtime as the boot sequence sees it: the crux.Runtime
// families plus crux.Host, the in-process wiring Hive installs (credential
// minters, launch-time resolvers, observers). Only the construction site and
// the installers that call Set* need it; everything else takes crux.Runtime
// or a narrower family. When the sidecar client lands (stage 3 of #14138)
// the Host half becomes callback endpoints and events, and this type goes.
type hostedRuntime interface {
	crux.Runtime
	crux.Host
}

// newHostedRuntime builds the runtime selected by CRUX_MODE (in-process by
// default) and asserts it exposes the Host wiring the boot sequence relies
// on. A sidecar mode without a Host is a boot error, not a silent downgrade.
func newHostedRuntime(cfg cruxconfig.RuntimeConfig, opts ...cruxruntime.Option) (hostedRuntime, error) {
	rt, err := cruxruntime.New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	hr, ok := rt.(hostedRuntime)
	if !ok {
		return nil, fmt.Errorf("crux runtime mode %q (%T): %w", cfg.Mode, rt, cruxruntime.ErrNotInProcess)
	}
	return hr, nil
}
