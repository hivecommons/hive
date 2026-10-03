package planengine

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
)

// Builder constructs an engine from the runs config.
type Builder func(cfg config.RunsConfig, logger *slog.Logger) (Engine, error)

var (
	buildersMu sync.RWMutex
	builders   = map[string]Builder{}
)

// Register registers the builder for a named planning engine. An engine
// package calls it from init(). Registering the same name twice panics, like
// worksource.RegisterAdditive: two builders for one name would make the
// selected engine depend on link order.
func Register(name string, build Builder) {
	buildersMu.Lock()
	defer buildersMu.Unlock()
	if _, dup := builders[name]; dup {
		panic(fmt.Sprintf("planengine: engine %q registered twice", name))
	}
	builders[name] = build
}

// Lookup returns the builder registered under name.
func Lookup(name string) (Builder, bool) {
	buildersMu.RLock()
	defer buildersMu.RUnlock()
	b, ok := builders[name]
	return b, ok
}

// Names returns the registered engine names in sorted order.
func Names() []string {
	buildersMu.RLock()
	defer buildersMu.RUnlock()
	names := make([]string, 0, len(builders))
	for n := range builders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func init() { config.RegisteredRunsEngines = Names }
