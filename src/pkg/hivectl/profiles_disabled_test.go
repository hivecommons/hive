package hivectl

import (
	"errors"
	"reflect"
	"testing"
)

func TestDisabledProfilesRetainCredentialsAndAlignedProjection(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	set := opsSet()
	original := append([]Profile(nil), set.Profiles...)
	for _, name := range []string{"ACME", "other"} {
		if err := set.SetDisabled(name, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(set); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range loaded.Profiles {
		if !p.Disabled {
			t.Fatalf("profile %d did not retain disabled state", i)
		}
		p.Disabled = false
		if !reflect.DeepEqual(p, original[i]) {
			t.Fatalf("profile %d changed credentials or metadata", i)
		}
	}
	env, err := readEnvFile(store.EnvPath())
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"HIVE_HUB":                "wss://acme.example/contribute,wss://other.example/contribute",
		"HIVE_REGISTRATION_TOKEN": "t1,t2",
		"CONTRIBUTOR_ID":          "c1,c2",
		"HIVE_HUB_DISABLED":       "true,true",
	} {
		if got := env.value(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, name := range []string{"acme", "other"} {
		if err := loaded.SetDisabled(name, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Commit(loaded); err != nil {
		t.Fatal(err)
	}
	env, err = readEnvFile(store.EnvPath())
	if err != nil {
		t.Fatal(err)
	}
	if env.value("HIVE_HUB_DISABLED") != "false,false" {
		t.Fatal("re-enabled hives remain excluded")
	}
	if !reflect.DeepEqual(loaded.Profiles, original) {
		t.Fatal("enable did not restore original profiles")
	}
	if err := loaded.SetDisabled("missing", true); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("missing profile: %v", err)
	}
}

func TestDisabledProjectionFollowsActiveOrder(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	set := opsSet()
	if err := set.SetDisabled("acme", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := set.Use("other"); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(set); err != nil {
		t.Fatal(err)
	}
	env, err := readEnvFile(store.EnvPath())
	if err != nil {
		t.Fatal(err)
	}
	if env.value("HIVE_HUB_DISABLED") != "false,true" || env.value("HIVE_REGISTRATION_TOKEN") != "t2,t1" || env.value("CONTRIBUTOR_ID") != "c2,c1" {
		t.Fatal("disabled flags and credentials did not follow profile ordering")
	}
}

func TestLegacyProfilesDefaultToEnabled(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	if err := store.Save(opsSet()); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range loaded.Profiles {
		if p.Disabled {
			t.Fatal("legacy profile disabled")
		}
	}
}
