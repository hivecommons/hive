package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
)

// customers.store shadow opens colony.db beside people.json; json never
// creates it.
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
	if got := customers.ParseStoreMode(cfg.CustomersStoreMode()); got != customers.StoreColony {
		t.Fatalf("default mode = %q, want colony (#19972)", got)
	}
	cfg.Customers.Store = "json"
	if got := customers.ParseStoreMode(cfg.CustomersStoreMode()); got != customers.StoreJSON {
		t.Fatalf("explicit json = %q, want json", got)
	}
	cfg.Customers.Store = "colony"
	if got := customers.ParseStoreMode(cfg.CustomersStoreMode()); got != customers.StoreColony {
		t.Fatalf("mode = %q", got)
	}
	cfg.Customers.Store = "nonsense"
	if got := customers.ParseStoreMode(cfg.CustomersStoreMode()); got != customers.StoreJSON {
		t.Fatalf("unknown mode = %q, want json", got)
	}
}

// A hive that ran on the old json default and is upgraded with nothing set
// boots on colony (#19972): the boot migration imports people.json, keeps the
// .pre-colony backup, and the records read the same.
func TestWireCustomersDefaultUpgradeFromJSON(t *testing.T) {
	t.Setenv(config.EnvCustomersStore, "")
	dir := t.TempDir()
	data := filepath.Join(dir, "customers")
	graph := filepath.Join(dir, "graph")
	svc, stop := wireCustomersAtMode(data, graph, customers.StoreJSON, nil, nil)
	if svc == nil {
		t.Fatal("json mode did not wire")
	}
	p, err := svc.svc.Add(ownerViewer, "Ada Example", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada-example"}})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	before, err := os.ReadFile(filepath.Join(data, customers.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, customers.ColonyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("json mode created colony.db: %v", err)
	}

	mode := customers.ParseStoreMode((&config.Config{}).CustomersStoreMode())
	if mode != customers.StoreColony {
		t.Fatalf("default mode = %q, want colony", mode)
	}
	svc, stop = wireCustomersAtMode(data, graph, mode, nil, nil)
	if svc == nil {
		t.Fatal("the default mode did not wire")
	}
	t.Cleanup(stop)
	if got := svc.svc.Store().Colony().Mode(); got != customers.StoreColony {
		t.Fatalf("colony mirror mode = %q, want colony", got)
	}
	backup, err := os.ReadFile(filepath.Join(data, customers.FileName+customers.PreColonySuffix))
	if err != nil || string(backup) != string(before) {
		t.Fatalf("the upgrade boot kept no .pre-colony copy of people.json as it stood: %v", err)
	}
	got, err := svc.svc.Store().Get(p.ID)
	if err != nil || got.Name != "Ada Example" {
		t.Fatalf("person after the upgrade = %+v, %v", got, err)
	}
	par, err := svc.svc.Store().ColonyParity()
	if err != nil || par.ColonyPeople != par.JSONPeople || par.Mismatches != 0 || par.Failures != 0 {
		t.Fatalf("parity after the upgrade = %+v, %v", par, err)
	}
}

// When the colony store cannot be used, the default mode falls back to
// people.json: the section still works and colony.db is left as it was.
func TestWireCustomersDefaultFallsBackToJSON(t *testing.T) {
	t.Setenv(config.EnvCustomersStore, "")
	dir := t.TempDir()
	data := filepath.Join(dir, "customers")
	graph := filepath.Join(dir, "graph")
	svc, stop := wireCustomersAtMode(data, graph, customers.StoreJSON, nil, nil)
	if svc == nil {
		t.Fatal("json mode did not wire")
	}
	p, err := svc.svc.Add(ownerViewer, "Bea Example", []customers.Identity{{Kind: customers.KindGitHub, Value: "bea-example"}})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	garbage := []byte("not a bbolt file")
	if err := os.WriteFile(filepath.Join(data, customers.ColonyFileName), garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	svc, stop = wireCustomersAtMode(data, graph, customers.ParseStoreMode((&config.Config{}).CustomersStoreMode()), nil, nil)
	if svc == nil {
		t.Fatal("a damaged colony.db took the Customers section down")
	}
	t.Cleanup(stop)
	if svc.svc.Store().Colony() != nil {
		t.Fatal("a damaged colony.db was attached")
	}
	if got, err := svc.svc.Store().Get(p.ID); err != nil || got.Name != "Bea Example" {
		t.Fatalf("json fallback read = %+v, %v", got, err)
	}
	if left, _ := os.ReadFile(filepath.Join(data, customers.ColonyFileName)); string(left) != string(garbage) {
		t.Fatal("the fallback rewrote colony.db")
	}
}
