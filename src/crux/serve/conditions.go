package serve

import (
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
)

type conditionTee struct{ s *Server }

// Conditions is the server's own condition reporter: every report reaches
// the event stream (EventCondition) and ConditionReporter when set. It is
// how a condition about the serving process itself, rather than one the
// runtime raises, gets onto the same path (hivecap.Watch uses it for
// hive_callback_unavailable).
func (s *Server) Conditions() crux.ConditionReporter { return conditionTee{s} }

// StreamConditions reports only to the event stream, never to
// ConditionReporter. It is for a condition that must not travel over the
// forwarding path because it describes that path being down
// (hivecap.Watch's hive_callback_unavailable, when ConditionReporter is the
// hive callback itself).
func (s *Server) StreamConditions() crux.ConditionReporter { return streamConditions{s} }

type streamConditions struct{ s *Server }

func (t streamConditions) Observe(class, key, message, diagnostics string) {
	t.s.publishData(api.EventCondition, "", api.ConditionEvent{Action: api.ConditionObserve, Class: class, Key: key, Message: message, Diagnostics: diagnostics})
}

func (t streamConditions) Clear(class, key string) {
	t.s.publishData(api.EventCondition, "", api.ConditionEvent{Action: api.ConditionClear, Class: class, Key: key})
}

func (t conditionTee) Observe(class, key, message, diagnostics string) {
	t.s.publishData(api.EventCondition, "", api.ConditionEvent{Action: api.ConditionObserve, Class: class, Key: key, Message: message, Diagnostics: diagnostics})
	if t.s.ConditionReporter != nil {
		t.s.ConditionReporter.Observe(class, key, message, diagnostics)
	}
}

func (t conditionTee) Clear(class, key string) {
	t.s.publishData(api.EventCondition, "", api.ConditionEvent{Action: api.ConditionClear, Class: class, Key: key})
	if t.s.ConditionReporter != nil {
		t.s.ConditionReporter.Clear(class, key)
	}
}
