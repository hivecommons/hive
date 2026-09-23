package extwork

import (
	"fmt"
	"sort"
	"sync"
)

// Engine-neutral setting keys a Factory reads. They are plain strings from
// hive.yaml and never carry a credential.
const (
	SettingEndpoint        = "endpoint"
	SettingWorkflowVersion = "workflow_version"
)

// Factory builds an Adapter from operator settings. Settings are plain strings
// (endpoint, workflow version) and never contain a credential.
type Factory func(settings map[string]string) (Adapter, error)

// Registry maps engine names to factories. Engine packages register themselves
// from a build-tag-gated file in the binary (cmd/hive/extwork_flue.go), so an
// engine that is not compiled in is simply absent here and can never be
// selected by configuration alone.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{factories: map[string]Factory{}} }

// DefaultRegistry is the process-wide registry the binary populates.
var DefaultRegistry = NewRegistry()

// Register adds a factory; a duplicate engine name is a programming error.
func (r *Registry) Register(engine string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[engine]; dup {
		panic(fmt.Sprintf("extwork: engine %q registered twice", engine))
	}
	r.factories[engine] = f
}

// Linked reports whether an engine was compiled into this binary.
func (r *Registry) Linked(engine string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[engine]
	return ok
}

// Engines lists the linked engines, sorted.
func (r *Registry) Engines() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for name := range r.factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ErrEngineNotLinked is returned by Open for an engine absent from the build.
var ErrEngineNotLinked = fmt.Errorf("extwork: engine not linked into this binary")

// Open builds the adapter for engine.
func (r *Registry) Open(engine string, settings map[string]string) (Adapter, error) {
	r.mu.RLock()
	f, ok := r.factories[engine]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrEngineNotLinked, engine)
	}
	return f(settings)
}
