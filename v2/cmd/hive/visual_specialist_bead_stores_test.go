package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

func TestEnsureVisualSpecialistBeadStoresRegistersOnlyFixedRoles(t *testing.T) {
	root := t.TempDir()
	existingDir := filepath.Join(root, "quality-existing")
	existing, err := beads.NewSharedStore(existingDir)
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"quality": existing}
	lifecycleDirs := map[string]string{"quality": existingDir}

	initialized, err := ensureVisualSpecialistBeadStores(stores, lifecycleDirs, root, "hive-test")
	if err != nil {
		t.Fatal(err)
	}
	wantInitialized := []string{"ci-maintainer", "sec-check", "architect", "scanner"}
	if !reflect.DeepEqual(initialized, wantInitialized) {
		t.Fatalf("initialized roles = %v, want %v", initialized, wantInitialized)
	}
	if stores["quality"] != existing || lifecycleDirs["quality"] != existingDir {
		t.Fatal("existing configured role store was replaced")
	}
	wantRoles := []string{"quality", "ci-maintainer", "sec-check", "architect", "scanner"}
	for _, role := range wantRoles {
		if stores[role] == nil || strings.TrimSpace(lifecycleDirs[role]) == "" {
			t.Errorf("fixed specialist role %q was not registered", role)
		}
	}
	for _, forbidden := range []string{"supervisor", "guide", "publisher", "untrusted-role"} {
		if stores[forbidden] != nil || lifecycleDirs[forbidden] != "" {
			t.Errorf("non-specialist role %q was registered", forbidden)
		}
	}
}

func TestEnsureVisualSpecialistBeadStoresFailsBeforePublishingPartialMap(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "ci-maintainer")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{}
	lifecycleDirs := map[string]string{}

	if _, err := ensureVisualSpecialistBeadStores(stores, lifecycleDirs, root, "hive-test"); err == nil ||
		!strings.Contains(err.Error(), `open Visual Hive specialist store "ci-maintainer"`) {
		t.Fatalf("invalid specialist store path was accepted: %v", err)
	}
	if len(stores) != 0 || len(lifecycleDirs) != 0 {
		t.Fatalf("failed provisioning published a partial map: stores=%v dirs=%v", stores, lifecycleDirs)
	}
}

func TestEnsureVisualSpecialistBeadStoresRejectsIncompleteExistingRegistration(t *testing.T) {
	stores := map[string]*beads.Store{"quality": &beads.Store{}}
	lifecycleDirs := map[string]string{}
	if _, err := ensureVisualSpecialistBeadStores(stores, lifecycleDirs, t.TempDir(), "hive-test"); err == nil ||
		!strings.Contains(err.Error(), `existing Visual Hive specialist store "quality" has no lifecycle directory`) {
		t.Fatalf("incomplete existing role registration was accepted: %v", err)
	}
}
