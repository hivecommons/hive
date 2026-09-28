package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// #9231: a hive booted with knowledge.enabled: false still connects the
// bead-synth vault and runs the synthesizer, but no primer exists, so its
// facts never reach a kick. Enabling from the dashboard must build a primer
// live that primes that vault, and disabling must unregister it.
func TestKnowledgePrimerControlTogglesPrimerLive(t *testing.T) {
	cfg := bootKnowledgeConfig(t)
	cfg.Knowledge.Enabled = false
	b := newBootKnowledgeBoot(t, cfg)
	b.beadStores["scanner"] = newTestBeadStore(t)
	b.bootKnowledgeWith(newBootKnowledgeFake(t).deps)
	ctl := knowledgePrimerControl{b: b}

	if b.sched.GetPrimer() != nil || ctl.KnowledgePrimerStatus().Registered {
		t.Fatal("knowledge.enabled: false must boot without a primer")
	}

	on := ctl.SetKnowledgePrimer(true)
	p := b.sched.GetPrimer()
	if p == nil || !on.Registered {
		t.Fatalf("enable: scheduler primer=%v status=%+v, want a registered primer", p != nil, on)
	}
	if got := strings.Join(p.FileStoreNames(), ","); got != "bead-synth-wiki" {
		t.Fatalf("enable: primer stores = %q, want the bead-synth vault", got)
	}
	if len(on.Sources) != 1 || on.Sources[0].Name != "bead-synth-wiki" || on.Sources[0].Kind != "store" {
		t.Fatalf("enable: status sources = %+v", on.Sources)
	}
	if on.RestartRequired {
		t.Fatalf("enable: nothing configured needs a restart, got %q", on.RestartReason)
	}

	if again := ctl.SetKnowledgePrimer(true); b.sched.GetPrimer() != p || !again.Registered {
		t.Fatal("re-enabling must keep the registered primer, not rebuild it")
	}

	off := ctl.SetKnowledgePrimer(false)
	if b.sched.GetPrimer() != nil || off.Registered || len(off.Sources) != 0 {
		t.Fatalf("disable: scheduler primer=%v status=%+v, want none", b.sched.GetPrimer() != nil, off)
	}
}

// The live primer cannot bring up what the file-only fallback API skipped at
// boot; the status must name it rather than report a clean enable.
func TestKnowledgePrimerControlRestartReason(t *testing.T) {
	t.Run("fallback API with configured layers and vaults", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		cfg.Knowledge.Enabled = false
		cfg.Knowledge.Layers = []config.KnowledgeLayer{{Type: "project", URL: "http://127.0.0.1:1"}}
		cfg.Knowledge.Vaults = []config.VaultConfig{{Name: "team-wiki", Path: t.TempDir()}}
		b := newBootKnowledgeBoot(t, cfg)
		b.bootKnowledgeWith(newBootKnowledgeFake(t).deps)

		st := knowledgePrimerControl{b: b}.SetKnowledgePrimer(true)
		if !st.Registered || !st.RestartRequired {
			t.Fatalf("status = %+v, want registered and restart_required", st)
		}
		for _, want := range []string{"1 configured vault", "1 configured wiki layer"} {
			if !strings.Contains(st.RestartReason, want) {
				t.Fatalf("restart reason %q does not mention %q", st.RestartReason, want)
			}
		}
		if len(st.Sources) != 1 || st.Sources[0].Kind != "wiki" {
			t.Fatalf("sources = %+v, want the configured wiki layer", st.Sources)
		}
	})
	t.Run("API built from config at boot", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		cfg.Knowledge.Enabled = true
		cfg.Knowledge.Layers = []config.KnowledgeLayer{{Type: "project", URL: "http://127.0.0.1:1"}}
		b := newBootKnowledgeBoot(t, cfg)
		b.bootKnowledgeWith(newBootKnowledgeFake(t).deps)

		st := knowledgePrimerControl{b: b}.SetKnowledgePrimer(true)
		if !st.Registered || st.RestartRequired {
			t.Fatalf("status = %+v, want registered with no restart", st)
		}
	})
}
