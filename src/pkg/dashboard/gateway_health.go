package dashboard

import (
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/inferencehealth"
)

type serverGatewayHealthStore = inferencehealth.Store

var (
	gatewayHealthMu sync.RWMutex
	gatewayHealthFn func() []inferencehealth.GatewayStatus
)

// SetGatewayHealthProvider registers the proxy's current per-gateway inference
// health snapshot. The dashboard adds Gateway Test results to the same heartbeat
// payload so both runtime failures and operator probes reach the hub.
func SetGatewayHealthProvider(fn func() []inferencehealth.GatewayStatus) {
	gatewayHealthMu.Lock()
	defer gatewayHealthMu.Unlock()
	gatewayHealthFn = fn
}

func getGatewayHealthFn() func() []inferencehealth.GatewayStatus {
	gatewayHealthMu.RLock()
	defer gatewayHealthMu.RUnlock()
	return gatewayHealthFn
}

func (s *Server) gatewayHealthStore() *inferencehealth.Store {
	if s == nil {
		return nil
	}
	s.gatewayHealthOnce.Do(func() { s.gatewayHealth = inferencehealth.NewStore() })
	return s.gatewayHealth
}

// GatewayHealthState returns active gateway faults learned from Gateway Test and
// from the local inference proxy. When both know the same gateway, the newest
// failure wins.
func (s *Server) GatewayHealthState() []inferencehealth.GatewayStatus {
	byName := map[string]inferencehealth.GatewayStatus{}
	add := func(items []inferencehealth.GatewayStatus) {
		for _, st := range items {
			name := strings.ToLower(strings.TrimSpace(st.Name))
			if name == "" || strings.TrimSpace(st.ErrorClass) == "" {
				continue
			}
			prev, ok := byName[name]
			if !ok || gatewayStatusAfter(st, prev) {
				byName[name] = st
			}
		}
	}
	if store := s.gatewayHealthStore(); store != nil {
		add(store.Snapshot())
	}
	if fn := getGatewayHealthFn(); fn != nil {
		add(fn())
	}
	out := make([]inferencehealth.GatewayStatus, 0, len(byName))
	for _, st := range byName {
		out = append(out, st)
	}
	inferencehealth.Sort(out)
	return out
}

func gatewayStatusAfter(a, b inferencehealth.GatewayStatus) bool {
	at, aerr := time.Parse(time.RFC3339, a.LastErrorAt)
	bt, berr := time.Parse(time.RFC3339, b.LastErrorAt)
	if aerr != nil || berr != nil {
		return a.LastErrorAt > b.LastErrorAt
	}
	return at.After(bt)
}
