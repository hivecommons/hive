package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/publish"
)

func TestPublicationPolicyDefaultOffAndRequiresOwner(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, config.ConvergenceModeEnforce)
	if pol := publicationPolicy(nil); pol.Enabled {
		t.Fatal("nil config must yield a disabled policy")
	}
	level := 6
	cfg := &config.Config{ACMMLevel: &level}
	if pol := publicationPolicy(cfg); pol.Enabled || pol.ACMMLevel != 6 || pol.Mode != config.ConvergenceModeEnforce {
		t.Fatalf("default policy = %+v, want disabled at L6 enforce", pol)
	}
	cfg.Publication.Enabled = true
	if pol := publicationPolicy(cfg); pol.Enabled {
		t.Fatal("enabled without a campaign owner must stay disabled")
	}
	cfg.Publication.Owner = "maintainer"
	if pol := publicationPolicy(cfg); !pol.Enabled {
		t.Fatalf("enabled with owner = %+v", pol)
	}
}

func TestPrivateChannelForMapsConfiguredChannel(t *testing.T) {
	if privateChannelFor(nil, nil, nil) != nil {
		t.Fatal("nil config must have no channel")
	}
	cfg := &config.Config{}
	if privateChannelFor(cfg, nil, nil) != nil {
		t.Fatal("empty channel must be nil")
	}
	cfg.Publication.PrivateChannel = "repo:acme/security"
	if privateChannelFor(cfg, nil, nil) != nil {
		t.Fatal("repo channel without an issue seam must be nil")
	}
	cfg.Publication.PrivateChannel = "notify"
	if privateChannelFor(cfg, nil, nil) != nil {
		t.Fatal("notify channel without a notifier must be nil")
	}
	cfg.Publication.PrivateChannel = "mailto:x@example.com"
	if privateChannelFor(cfg, nil, nil) != nil {
		t.Fatal("unroutable channel must be nil")
	}
	notifierChannel{}.Send("t", "m") // nil notifier is a no-op
}

func TestBuildFindingPublisherIsInertWithoutBoundary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	if p, l := buildFindingPublisher(cfg, nil, nil, nil, nil, logger); p != nil || l != nil {
		t.Fatal("no boundary means no publisher")
	}
	if p, _ := buildFindingPublisher(nil, &mutation.Boundary{}, nil, nil, nil, logger); p != nil {
		t.Fatal("no config means no publisher")
	}
	cfg.Publication.Owner = "maintainer"
	p, _ := buildFindingPublisher(cfg, &mutation.Boundary{Executor: mutation.Executor{Mode: config.ConvergenceModeShadow}}, nil, nil, nil, logger)
	if p == nil || p.Actor != "maintainer" || p.Issues != nil || p.Private != nil {
		t.Fatalf("publisher = %+v", p)
	}
	if pol := p.PolicyFunc(); pol.Enabled {
		t.Fatalf("publisher must be off by default: %+v", pol)
	}
	if p.Classify(publish.Finding{Title: "possible secret in logs"}) != publish.SensitivitySensitive {
		t.Fatal("conservative classifier must be wired")
	}
}

func TestWireFindingPublisherNilSafe(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := &boot{logger: logger}
	b.wireFindingPublisher()
	if b.findingPublisher != nil {
		t.Fatal("a boot without config or boundary must not build a publisher")
	}
	b.cfg = &config.Config{}
	b.mutationBoundary = &mutation.Boundary{Executor: mutation.Executor{Mode: config.ConvergenceModeShadow}}
	b.wireFindingPublisher()
	if b.findingPublisher == nil {
		t.Fatal("a boot with a boundary must build the publisher")
	}
}
