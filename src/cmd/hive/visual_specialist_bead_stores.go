package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kubestellar/hive/pkg/beads"
)

// ensureVisualSpecialistBeadStores registers the five fixed specialist role
// stores used by verified Visual Hive routing. It does not enable or start an
// agent. Provisioning is called only when an installed Visual Hive contract is
// present, so ordinary Hive startup remains unchanged when Visual Hive is
// dormant.
func ensureVisualSpecialistBeadStores(
	stores map[string]*beads.Store,
	lifecycleDirs map[string]string,
	root string,
	hiveID string,
) ([]string, error) {
	if stores == nil || lifecycleDirs == nil {
		return nil, fmt.Errorf("Visual Hive specialist bead-store maps are required")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("Visual Hive specialist beads root is required")
	}

	pendingStores := make(map[string]*beads.Store)
	pendingDirs := make(map[string]string)
	initialized := make([]string, 0, len(specialistRuntimeRoles()))
	for _, specialist := range specialistRuntimeRoles() {
		role := string(specialist)
		if stores[role] != nil {
			if strings.TrimSpace(lifecycleDirs[role]) == "" {
				return nil, fmt.Errorf("existing Visual Hive specialist store %q has no lifecycle directory", role)
			}
			continue
		}
		dir := filepath.Join(root, role)
		store, err := beads.NewSharedStore(dir)
		if err != nil {
			return nil, fmt.Errorf("open Visual Hive specialist store %q: %w", role, err)
		}
		store.SetHiveID(hiveID)
		pendingStores[role] = store
		pendingDirs[role] = dir
		initialized = append(initialized, role)
	}

	for role, store := range pendingStores {
		stores[role] = store
		lifecycleDirs[role] = pendingDirs[role]
	}
	return initialized, nil
}
