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
	if err != nil || par.JSONPeople != 1 || par.ColonyPeople != 1 || par.Mismatches != 0 {
		t.Fatalf("parity = %+v, %v", par, err)
	}
	if _, err := svc.svc.Forget(ownerViewer, p.ID); err != nil {
		t.Fatal(err)
	}
	if par, _ := svc.svc.Store().ColonyParity(); par.ColonyPeople != 0 {
		t.Fatalf("forget left the person in colony: %+v", par)
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
