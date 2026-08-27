package main

import (
	"regexp"
	"strings"
	"testing"
)

// An operator-provided HIVE_ID must win over anything on disk or any
// generated name — it is the fleet-facing identity, and silently replacing it
// would detach the spoke from its hub registration.
func TestLoadOrGenerateHiveIDEnvWins(t *testing.T) {
	t.Setenv("HIVE_ID", "hive-operator-chosen")
	if got := loadOrGenerateHiveID(restoreTestLogger()); got != "hive-operator-chosen" {
		t.Errorf("loadOrGenerateHiveID = %q, want the HIVE_ID env value", got)
	}
}

// Without an env override the function must still return a usable identity:
// non-empty and free of surrounding whitespace (the on-disk form carries a
// trailing newline that must never leak into the ID itself).
func TestLoadOrGenerateHiveIDAlwaysUsable(t *testing.T) {
	t.Setenv("HIVE_ID", "")
	got := loadOrGenerateHiveID(restoreTestLogger())
	if got == "" {
		t.Fatal("loadOrGenerateHiveID returned an empty identity")
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("loadOrGenerateHiveID = %q carries surrounding whitespace", got)
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
