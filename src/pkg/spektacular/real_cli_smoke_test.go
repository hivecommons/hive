//go:build integration

package spektacular

// TestRealSpektacularCLISmoke is the last confirmed divergence #10074 called
// out: "nothing in the repo exercises the pinned release binary." Every
// other test in this package drives the Runner through the in-tree fake
// (testdata/spektacular-fake/spektacular) or a scripted Exec stub, so a
// mismatch between those stand-ins and the real 0.22.0 release could go
// unnoticed. This test runs the Confirmed contract in docs/spektacular.md
// (#8301, jumppad-labs/spektacular#45) against whatever binary
// SPEKTACULAR_REAL_CLI_BIN names, or the first `spektacular` on PATH, and is
// skipped everywhere else: it needs the `integration` build tag AND a real
// binary, so `go test ./...` (and every other package test) never depends on
// it. The spektacular-real-cli-smoke workflow downloads the release pinned in
// src/Dockerfile.contributor and sets SPEKTACULAR_EXPECTED_VERSION to it.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func realSpektacularBinary(t *testing.T) string {
	t.Helper()
	if bin := strings.TrimSpace(os.Getenv("SPEKTACULAR_REAL_CLI_BIN")); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("spektacular")
	if err != nil {
		t.Skip("set SPEKTACULAR_REAL_CLI_BIN to a real spektacular binary (or put one on PATH) to run the real-CLI smoke (#10074)")
	}
	return bin
}

func TestRealSpektacularCLISmoke(t *testing.T) {
	bin := realSpektacularBinary(t)
	ctx := context.Background()

	// Confirmed #1 (sort of): --version answers
	// "spektacular version <version> (<commit>)" so Probe can run against it
	// (#10269). The in-tree fake prints the same shape.
	probed, err := Probe(ctx, bin)
	if err != nil || !probed.Present {
		t.Fatalf("Probe(%s): present=%v err=%v", bin, probed.Present, err)
	}
	fields := strings.Fields(probed.Version)
	if len(fields) < 3 || fields[0] != "spektacular" || fields[1] != "version" {
		t.Fatalf("--version printed %q, want the `spektacular version <version> (<commit>)` shape", probed.Version)
	}
	// The workflow exports the pin it downloaded, so the smoke fails loudly
	// if it ever runs a binary other than the pinned release.
	if want := strings.TrimSpace(os.Getenv("SPEKTACULAR_EXPECTED_VERSION")); want != "" {
		if got := strings.TrimPrefix(fields[2], "v"); got != strings.TrimPrefix(want, "v") {
			t.Fatalf("--version reported %q, want the pinned %q", got, want)
		}
	}

	dir := t.TempDir()
	run(t, ctx, bin, dir, "init", "copilot", "--name", "hive-real-cli-smoke")

	r := &Runner{Exec: BinaryExec(bin)}

	// Confirmed #2: a bare slug that was never created is artifact_not_found,
	// store-backed exactly as #10074 asked the fake to encode, resolved
	// through the same statusInDir path poll.go drives in production.
	if _, err := r.statusInDir(ctx, dir, KindSpec, "ghost-of-a-spec"); err == nil {
		t.Fatalf("status of an uncreated spec: got nil error, want artifact_not_found")
	} else {
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("status of an uncreated spec: got %v (%T), want *NotFoundError", err, err)
		}
	}

	// `spec new` creates a timestamp-prefixed artifact id, never the bare
	// slug Hive asked for (the naming mismatch #10074's "Observed" bullet
	// described).
	newOut := run(t, ctx, bin, dir, "spec", "new", "--data", `{"name":"demo"}`)
	var created struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(newOut, &created); err != nil || created.Name == "" {
		t.Fatalf("spec new output %q did not decode a name: %v", newOut, err)
	}
	if created.Name == "demo" {
		t.Fatalf("spec new returned the bare slug %q; want a timestamp-prefixed id", created.Name)
	}

	// Confirmed #1: the status verb's draft/final contract, read by the
	// bare-name join key production code uses.
	status, err := r.statusInDir(ctx, dir, KindSpec, created.Name)
	if err != nil {
		t.Fatalf("status of the created spec: %v", err)
	}
	if status.DocumentStatus != DocumentDraft {
		t.Fatalf("new spec document_status = %q, want draft", status.DocumentStatus)
	}

	// ResolveArtifact is the production path (poll.go) that turns Hive's
	// stable slug into the CLI's timestamped id via `spec file list`,
	// exercised here against the real store instead of a Go stub.
	resolved, err := r.ResolveArtifact(ctx, dir, KindSpec, "demo")
	if err != nil {
		t.Fatalf("ResolveArtifact(demo): %v", err)
	}
	if resolved != created.Name {
		t.Fatalf("ResolveArtifact(demo) = %q, want %q", resolved, created.Name)
	}

	// `spec file read` through the real store, the other half of the
	// resolver/file-list path #10074 said was reachable only via a Go stub.
	body, err := r.readSpecInDir(ctx, dir, created.Name)
	if err != nil {
		t.Fatalf("readSpecInDir(%s): %v", created.Name, err)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatalf("readSpecInDir(%s) returned an empty document", created.Name)
	}
}

func run(t *testing.T, ctx context.Context, bin, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", bin, strings.Join(args, " "), err, out)
	}
	return out
}
