package client

import (
	"net/http"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/watchdog"
)

// ---- watchdog.Source over the wire ----

// WatchdogSource returns the server's watchdog seam, the way
// *agent.Manager.WatchdogSource does in-process, so crux/runtime's
// NewWatchdogFleet can compose a fleet for a sidecar.
func (r *Runtime) WatchdogSource() watchdog.Source { return source{r} }

type source struct{ r *Runtime }

func (s source) AgentNames() []string {
	ctx, cancel := s.r.ctx()
	defer cancel()
	return s.r.names(ctx, "WatchdogSource.AgentNames", http.MethodGet, api.PathWatchdogAgents)
}

func (s source) observe(name string) (api.WatchdogObservation, error) {
	var o api.WatchdogObservation
	err := s.r.call(http.MethodGet, api.WatchdogPath(esc(name)), nil, &o)
	return o, err
}

func (s source) Observe(name string) (watchdog.Observation, error) {
	o, err := s.observe(name)
	if err != nil {
		return watchdog.Observation{}, err
	}
	return watchdog.Observation{
		Backend: o.Backend, SessionExists: o.SessionExists, Pane: o.Pane, HasCLIMarker: o.HasCLIMarker,
		ShowsLoginPrompt: o.ShowsLoginPrompt, LastChange: o.LastChange, StartedAt: o.StartedAt,
		AuthAvailable: o.AuthAvailable, AuthKnown: o.AuthKnown, CredentialProven: o.CredentialProven,
		ProviderErrorClass: o.ProviderErrorClass, ProviderErrorLine: o.ProviderErrorLine,
	}, nil
}

func (s source) LastProduction(name string) (time.Time, bool) {
	o, err := s.observe(name)
	if err != nil || o.LastProduction == nil {
		return time.Time{}, false
	}
	return *o.LastProduction, true
}

func (s source) SetConditions(name string, conds []crux.Condition) {
	wire := make([]api.Condition, 0, len(conds))
	for _, c := range conds {
		wire = append(wire, api.Condition{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message, LastTransitionTime: c.LastTransitionTime})
	}
	if err := s.r.call(http.MethodPut, api.WatchdogConditionsPath(esc(name)), api.Conditions{Conditions: wire}, nil); err != nil {
		s.r.drop("WatchdogSource.SetConditions", err, "agent", name)
	}
}
