package planengine

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestRegisterLookup(t *testing.T) {
	const name = "test-engine-lookup"
	if _, ok := Lookup(name); ok {
		t.Fatalf("Lookup(%q) found a builder before Register", name)
	}
	called := false
	Register(name, func(config.RunsConfig, *slog.Logger) (Engine, error) {
		called = true
		return nil, nil
	})
	build, ok := Lookup(name)
	if !ok || build == nil {
		t.Fatalf("Lookup(%q) = %v, %v after Register", name, build, ok)
	}
	if _, err := build(config.RunsConfig{}, slog.Default()); err != nil || !called {
		t.Fatalf("looked-up builder was not the registered one (called=%v, err=%v)", called, err)
	}
	if _, ok := Lookup("test-engine-unknown"); ok {
		t.Fatal("Lookup of an unregistered name should report false")
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	const name = "test-engine-dup"
	Register(name, func(config.RunsConfig, *slog.Logger) (Engine, error) { return nil, nil })
	defer func() {
		if recover() == nil {
			t.Fatal("second Register with the same name should panic")
		}
	}()
	Register(name, func(config.RunsConfig, *slog.Logger) (Engine, error) { return nil, nil })
}
