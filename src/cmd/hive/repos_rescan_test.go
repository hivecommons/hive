package main

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A hive booted without usable GitHub credentials has nothing to enumerate.
// The rescan must say that plainly: reporting a "successful" scan of zero
// issues and zero PRs would repaint the cards empty and read as "your repos
// went quiet" rather than "this hive cannot see GitHub".
func TestRescanRepos_NoCredentials(t *testing.T) {
	var last atomic.Pointer[github.ActionableResult]
	var refreshed bool

	got, err := rescanRepos(context.Background(), &config.Config{}, nil, &last, func() { refreshed = true }, slog.Default())

	if !errors.Is(err, errNoForgeCredentials) {
		t.Fatalf("err = %v, want errNoForgeCredentials", err)
	}
	if got != nil {
		t.Errorf("result = %+v, want nil", got)
	}
	if refreshed {
		t.Error("published a dashboard refresh for a scan that never happened")
	}
	if last.Load() != nil {
		t.Error("overwrote the cached actionable result with nothing")
	}
}

// The boot-time reader and both writers of the cached enumeration must agree
// on one path, or a restart repaints from a file nothing is writing.
func TestLastActionablePathIsTheDataPVCFile(t *testing.T) {
	if lastActionablePath != "/data/last-actionable.json" {
		t.Fatalf("lastActionablePath = %q; the /data PVC restore path changed", lastActionablePath)
	}
}
