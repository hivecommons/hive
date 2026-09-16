package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func useHiveIDFilePath(t *testing.T, path string) {
	t.Helper()
	original := hiveIDFilePath
	hiveIDFilePath = path
	t.Cleanup(func() { hiveIDFilePath = original })
}

// An operator-provided HIVE_ID must win over anything on disk or any
// generated name — it is the fleet-facing identity, and silently replacing it
// would detach the spoke from its hub registration.
func TestLoadOrGenerateHiveIDEnvWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive-id")
	useHiveIDFilePath(t, path)
	t.Setenv("HIVE_ID", "hive-operator-chosen")
	if got := loadOrGenerateHiveID(restoreTestLogger()); got != "hive-operator-chosen" {
		t.Errorf("loadOrGenerateHiveID = %q, want the HIVE_ID env value", got)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "hive-operator-chosen\n" {
		t.Errorf("persisted HIVE_ID = %q, %v; want hive-operator-chosen newline", data, err)
	}
}

func TestLoadOrGenerateHiveIDEnvWinsWhenPersistFails(t *testing.T) {
	useHiveIDFilePath(t, t.TempDir())
	t.Setenv("HIVE_ID", "hive-operator-chosen")
	if got := loadOrGenerateHiveID(restoreTestLogger()); got != "hive-operator-chosen" {
		t.Errorf("loadOrGenerateHiveID = %q, want the HIVE_ID env value", got)
	}
}

// Without an env override the function must still return a usable identity:
// non-empty and free of surrounding whitespace (the on-disk form carries a
// trailing newline that must never leak into the ID itself).
func TestLoadOrGenerateHiveIDAlwaysUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive-id")
	useHiveIDFilePath(t, path)
	t.Setenv("HIVE_ID", "")
	got := loadOrGenerateHiveID(restoreTestLogger())
	if got == "" {
		t.Fatal("loadOrGenerateHiveID returned an empty identity")
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("loadOrGenerateHiveID = %q carries surrounding whitespace", got)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != got+"\n" {
		t.Errorf("generated ID file = %q, %v; want returned ID plus newline", data, err)
	}
	if stable := loadOrGenerateHiveID(restoreTestLogger()); stable != got {
		t.Errorf("second loadOrGenerateHiveID = %q, want stable generated ID %q", stable, got)
	}
}

func TestLoadOrGenerateHiveIDLoadsExistingDiskID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive-id")
	useHiveIDFilePath(t, path)
	t.Setenv("HIVE_ID", "")
	if err := os.WriteFile(path, []byte("  hive-from-disk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadOrGenerateHiveID(restoreTestLogger()); got != "hive-from-disk" {
		t.Errorf("loadOrGenerateHiveID = %q, want trimmed disk ID", got)
	}
}

func TestLoadOrGenerateHiveIDRegeneratesEmptyDiskID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive-id")
	useHiveIDFilePath(t, path)
	t.Setenv("HIVE_ID", "")
	if err := os.WriteFile(path, []byte(" \n\t"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadOrGenerateHiveID(restoreTestLogger())
	if !strings.HasPrefix(got, "hive-") {
		t.Errorf("loadOrGenerateHiveID = %q, want generated hive-* ID", got)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != got+"\n" {
		t.Errorf("regenerated ID file = %q, %v; want returned ID plus newline", data, err)
	}
}

func TestLoadOrGenerateHiveIDReturnsGeneratedIDWhenPersistFails(t *testing.T) {
	useHiveIDFilePath(t, t.TempDir())
	t.Setenv("HIVE_ID", "")
	got := loadOrGenerateHiveID(restoreTestLogger())
	if !strings.HasPrefix(got, "hive-") {
		t.Errorf("loadOrGenerateHiveID = %q, want generated hive-* ID", got)
	}
}

// randomName must always produce a well-formed Docker-style adjective-noun
// name — it feeds the generated "hive-<name>" identity, which downstream
// consumers treat as a single hostname-safe token.
func TestRandomNameFormat(t *testing.T) {
	wellFormed := regexp.MustCompile(`^[a-z]+-[a-z]+$`)
	// A handful of draws exercises the random path; every draw must be valid.
	const draws = 32
	for i := 0; i < draws; i++ {
		name := randomName()
		if !wellFormed.MatchString(name) {
			t.Fatalf("randomName() = %q, want lowercase adjective-noun", name)
		}
	}
}
