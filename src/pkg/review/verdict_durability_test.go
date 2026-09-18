package review

import (
	"os"
	"path/filepath"
	"testing"
)

// The verdict artifact is the record of what has already been judged. These
// tests pin the durability guarantees that keep a restarted hive from
// re-reviewing — and re-commenting on — PRs it has already handled.

func TestReviewVerdictsPathIsDurable(t *testing.T) {
	if got := filepath.Dir(ReviewVerdictsPath); got != DefaultDispatchStateDir {
		t.Fatalf("verdicts must live on the durable data dir alongside the dispatch state; got %q, want %q", got, DefaultDispatchStateDir)
	}
	if ReviewVerdictsPath == LegacyReviewVerdictsPath {
		t.Fatal("durable and legacy verdict paths must differ, otherwise the migration fallback is a no-op")
	}
}

func TestLoadArtifactFallsBackToLegacyPath(t *testing.T) {
	// An upgrading hive has verdicts only at the old location. It must keep
	// them, or it re-reviews everything it already judged.
	dir := t.TempDir()
	durable := filepath.Join(dir, "durable", ReviewVerdictsFile)
	legacy := filepath.Join(dir, "legacy", ReviewVerdictsFile)

	want := Artifact{Items: []Aggregate{{Repo: "projectbluefin/common", Number: 1011, HeadSHA: "abc123"}}}
	if err := WriteArtifact(legacy, want); err != nil {
		t.Fatalf("seed legacy artifact: %v", err)
	}

	restore := swapVerdictPaths(t, durable, legacy)
	defer restore()

	got, err := LoadArtifact("")
	if err != nil {
		t.Fatalf("LoadArtifact should fall back to the legacy path: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Number != 1011 || got.Items[0].HeadSHA != "abc123" {
		t.Fatalf("legacy verdicts not preserved across migration: %+v", got.Items)
	}
}

func TestLoadArtifactPrefersDurablePath(t *testing.T) {
	// Once migrated, the durable copy is authoritative; a stale legacy file
	// left behind on the ephemeral layer must not shadow it.
	dir := t.TempDir()
	durable := filepath.Join(dir, "durable", ReviewVerdictsFile)
	legacy := filepath.Join(dir, "legacy", ReviewVerdictsFile)

	if err := WriteArtifact(legacy, Artifact{Items: []Aggregate{{Number: 1, HeadSHA: "stale"}}}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if err := WriteArtifact(durable, Artifact{Items: []Aggregate{{Number: 2, HeadSHA: "fresh"}}}); err != nil {
		t.Fatalf("seed durable: %v", err)
	}

	restore := swapVerdictPaths(t, durable, legacy)
	defer restore()

	got, err := LoadArtifact("")
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].HeadSHA != "fresh" {
		t.Fatalf("durable verdicts must win over the legacy copy: %+v", got.Items)
	}
}

func TestLoadArtifactExplicitPathDoesNotFallBack(t *testing.T) {
	// An explicit path is a deliberate choice by the caller. Silently reading
	// some other file would make tests and tooling read the wrong verdicts.
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy", ReviewVerdictsFile)
	if err := WriteArtifact(legacy, Artifact{Items: []Aggregate{{Number: 7}}}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	restore := swapVerdictPaths(t, filepath.Join(dir, "durable", ReviewVerdictsFile), legacy)
	defer restore()

	if _, err := LoadArtifact(filepath.Join(dir, "missing", ReviewVerdictsFile)); !os.IsNotExist(err) {
		t.Fatalf("explicit missing path must report not-exist, got %v", err)
	}
}

func TestWriteArtifactRoundTripsThroughDurablePath(t *testing.T) {
	// The write side must target the durable path too, or nothing is ever
	// persisted there for the next boot to find.
	dir := t.TempDir()
	durable := filepath.Join(dir, "durable", ReviewVerdictsFile)

	restore := swapVerdictPaths(t, durable, filepath.Join(dir, "legacy", ReviewVerdictsFile))
	defer restore()

	want := Artifact{Items: []Aggregate{{Repo: "projectbluefin/bluefin", Number: 42, HeadSHA: "deadbeef"}}}
	if err := WriteArtifact("", want); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	if _, err := os.Stat(durable); err != nil {
		t.Fatalf("artifact was not written to the durable path: %v", err)
	}

	got, err := LoadArtifact("")
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Number != 42 {
		t.Fatalf("round trip lost the verdict: %+v", got.Items)
	}
}

func swapVerdictPaths(t *testing.T, durable, legacy string) func() {
	t.Helper()
	origDurable, origLegacy := ReviewVerdictsPath, LegacyReviewVerdictsPath
	ReviewVerdictsPath, LegacyReviewVerdictsPath = durable, legacy
	return func() { ReviewVerdictsPath, LegacyReviewVerdictsPath = origDurable, origLegacy }
}
