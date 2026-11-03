package main

import (
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/runtimeobs"
)

// startRuntimeObservability wires pkg/runtimeobs into the boot: the soft
// memory limit, the loopback-only pprof listener (debug.pprof_addr) and the
// periodic `mem:` log line (hivecommons/hive#14681). Nothing here is fatal —
// a bind failure only costs the profiles, never the hive.
func (b *boot) startRuntimeObservability() {
	out := runtimeobs.ApplyMemoryLimit(nil, nil)
	b.logger.Info("runtime: memory limit",
		"source", out.Source,
		"gomemlimit", out.Limit,
		"cgroup_limit", out.CgroupLimit,
		"percent", runtimeobs.MemoryLimitPercent,
	)

	addr := b.cfg.Debug.EffectivePprofAddr()
	if addr != "" && !runtimeobs.IsLoopbackAddr(addr) {
		b.logger.Warn("runtime: debug.pprof_addr is not a loopback address; using the default",
			"configured", addr, "default", config.DefaultPprofAddr)
		addr = config.DefaultPprofAddr
	}
	bound, stop, err := runtimeobs.StartPprof(addr, b.logger)
	switch {
	case err != nil:
		b.logger.Warn("runtime: pprof listener not started", "addr", addr, "error", err)
	case bound == "":
		b.logger.Info("runtime: pprof listener disabled", "key", "debug.pprof_addr")
	default:
		b.cleanup.push(stop)
		b.logger.Info("runtime: pprof listening (loopback only)", "addr", bound, "path", "/debug/pprof/")
	}

	runtimeobs.StartLogger(b.ctx, b.logger, nil)
	b.logger.Info("mem: runtime snapshot", runtimeobs.Take().LogAttrs()...)
}
