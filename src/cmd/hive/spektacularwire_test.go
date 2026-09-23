package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
)

func TestWireSpektacularRunner_DefaultOffAndOptIn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	if wireSpektacularRunner(&config.Config{}, srv, logger) {
		t.Fatal("runner installed with the feature off")
	}
	if wireSpektacularRunner(nil, srv, logger) || wireSpektacularRunner(&config.Config{}, nil, logger) {
		t.Fatal("nil inputs installed a runner")
	}
	cfg := &config.Config{Runs: config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true, Binary: "false"}}}
	if !wireSpektacularRunner(cfg, srv, logger) {
		t.Fatal("runner not installed with the feature on")
	}
	if !wireSpektacularRunner(cfg, srv, nil) {
		t.Fatal("nil logger prevented installation")
	}
}
