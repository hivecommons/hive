package spektacular

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/planengine"
)

func TestRegister_SpektacularIsTheRegisteredEngine(t *testing.T) {
	build, ok := planengine.Lookup("spektacular")
	if !ok || build == nil {
		t.Fatal(`planengine.Lookup("spektacular") found no builder`)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name       string
		cfg        config.SpektacularConfig
		wantBinary string
	}{
		{name: "default binary", cfg: config.SpektacularConfig{}, wantBinary: config.DefaultSpektacularBinary},
		{name: "configured binary", cfg: config.SpektacularConfig{Binary: "/opt/bin/spektacular", PollIntervalS: 7}, wantBinary: "/opt/bin/spektacular"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, err := build(config.RunsConfig{Spektacular: tc.cfg}, logger)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			e, ok := eng.(*Engine)
			if !ok {
				t.Fatalf("builder returned %T, want *spektacular.Engine", eng)
			}
			if e.Name() != "spektacular" || e.ContractRevision() != "spektacular-status/v1" {
				t.Fatalf("engine identity = %q %q", e.Name(), e.ContractRevision())
			}
			if e.binary != tc.wantBinary {
				t.Fatalf("engine binary = %q, want %q", e.binary, tc.wantBinary)
			}
			if e.runner == nil || e.runner.Exec == nil {
				t.Fatal("registered engine has no CLI exec")
			}
		})
	}
}
