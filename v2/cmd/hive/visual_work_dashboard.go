package main

import (
	"errors"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/dashboard"
)

// normalVisualDashboardGate binds the embedded Visual service to the existing
// dashboard's observed HTTP listener. The durable marker is for doctor and
// restart inspection; the in-process probe also follows live listener state so
// a later serve failure quiesces the service immediately.
type normalVisualDashboardGate struct {
	server     *dashboard.Server
	stateDir   string
	repository string

	mu         sync.RWMutex
	ready      bool
	generation string
}

func newNormalVisualDashboardGate(server *dashboard.Server, stateDir, repository string) (*normalVisualDashboardGate, error) {
	if server == nil || stateDir == "" || repository == "" {
		return nil, errors.New("normal Visual dashboard gate requires server, state, and repository bindings")
	}
	return &normalVisualDashboardGate{server: server, stateDir: stateDir, repository: repository}, nil
}

// Activate is called only after WaitForHTTP and MarkReadyIfListening succeed.
func (gate *normalVisualDashboardGate) Activate(now time.Time) error {
	if gate == nil || gate.server == nil || !gate.server.ListenerServing() {
		return errors.New("ordinary Hive dashboard listener is not serving")
	}
	marker, err := writeNormalVisualDashboardReady(gate.stateDir, gate.repository, gate.server.ListenerPort(), now)
	if err != nil {
		return err
	}
	gate.mu.Lock()
	gate.generation = marker.Generation
	gate.ready = gate.server.ListenerServing()
	ready := gate.ready
	gate.mu.Unlock()
	if !ready {
		_ = removeNormalVisualDashboardReady(gate.stateDir, marker.Generation)
		return errors.New("ordinary Hive dashboard listener stopped before readiness activation")
	}
	return nil
}

func (gate *normalVisualDashboardGate) Ready() bool {
	if gate == nil || gate.server == nil || !gate.server.ListenerServing() {
		return false
	}
	gate.mu.RLock()
	defer gate.mu.RUnlock()
	return gate.ready
}

func (gate *normalVisualDashboardGate) Stop() error {
	if gate == nil {
		return nil
	}
	gate.mu.Lock()
	gate.ready = false
	generation := gate.generation
	gate.mu.Unlock()
	if generation == "" {
		return nil
	}
	return removeNormalVisualDashboardReady(gate.stateDir, generation)
}
