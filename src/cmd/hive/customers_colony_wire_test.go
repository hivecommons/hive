package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
)

// customers.store shadow opens colony.db beside people.json; the default,
// json, never creates it.
func TestWireCustomersStoreMode(t *testing.T) {
	t.Setenv(config.EnvCustomersStore, "")
	dir := t.TempDir()
	data := filepath.Join(dir, "customers")
	svc, stop := wireCustomersAtMode(data, filepath.Join(dir, "graph"), customers.StoreJSON, nil, nil)
	if svc == nil {
		t.Fatal("json mode did not wire")
	}
	stop()
	if _, err := os.Stat(filepath.Join(data, customers.ColonyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("json mode created colony.db: %v", err)
	}

	svc, stop = wireCustomersAtMode(data, filepath.Join(dir, "graph"), customers.StoreShadow, nil, nil)
	if svc == nil {
		t.Fatal("shadow mode did not wire")
	}
	t.Cleanup(stop)
	if svc.svc.Store().Colony().Mode() != customers.StoreShadow {
		t.Fatal("shadow mode attached no colony store")
	}
	if _, err := os.Stat(filepath.Join(data, customers.ColonyFileName)); err != nil {
		t.Fatalf("shadow mode: %v", err)
	}
	p, err := svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	par, err := svc.svc.Store().ColonyParity()
	if err != nil || par.JSONPeople == 0 || par.ColonyPeople != par.JSONPeople || par.Mismatches != 0 {
		t.Fatalf("parity = %+v, %v", par, err)
	}
	if _, err := svc.svc.Forget(ownerViewer, p.ID); err != nil {
		t.Fatal(err)
	}
	// The forgotten person is gone, and the two stores still agree (the
	// viewer's own dashboard profile may have been created meanwhile).
	if _, err := svc.svc.Store().Get(p.ID); !errors.Is(err, customers.ErrNotFound) {
		t.Fatalf("the forgotten person is still in people.json: %v", err)
	}
	after, err := svc.svc.Store().ColonyParity()
	if err != nil || after.JSONPeople != after.ColonyPeople || after.JSONIdentities != after.ColonyIdent || after.Mismatches != 0 {
		t.Fatalf("parity after forget = %+v (before %+v), %v", after, par, err)
	}
	if !svc.svc.Store().Forgotten(customers.KindGitHub, "ada") {
		t.Fatal("the forgotten login has no tombstone")
	}
}

func TestCustomersStoreModeFromConfig(t *testing.T) {
	t.Setenv(config.EnvCustomersStore, "")
	cfg := &config.Config{}
	cfg.Customers.Store = "colony"
	if got := customers.ParseStoreMode(cfg.CustomersStoreMode()); got != customers.StoreColony {
		t.Fatalf("mode = %q", got)
	}
	cfg.Customers.Store = "nonsense"
	if got := customers.ParseStoreMode(cfg.CustomersStoreMode()); got != customers.StoreJSON {
		t.Fatalf("unknown mode = %q, want json", got)
	}
}
