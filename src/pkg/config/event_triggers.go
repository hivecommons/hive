package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrTriggerRulesChanged = errors.New("event triggers changed; reload before saving")

// TriggerRulesSnapshot shares Save's lock so evaluators never observe an
// unpersisted settings edit. Callers own the returned slice.
func (c *Config) TriggerRulesSnapshot() []TriggerRule {
	saveMu.Lock()
	defer saveMu.Unlock()
	return append([]TriggerRule{}, c.Triggers...)
}

func TriggerRulesVersion(rules []TriggerRule) string {
	data, _ := json.Marshal(append([]TriggerRule{}, rules...))
	return fmt.Sprintf(`"%x"`, sha256.Sum256(data))
}

// SaveTriggerRules compares only the trigger revision, leaving unrelated
// settings intact, and publishes to evaluators only after Save succeeds.
func (c *Config) SaveTriggerRules(version string, rules []TriggerRule) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	if version != TriggerRulesVersion(c.Triggers) {
		return ErrTriggerRulesChanged
	}
	next := *c
	next.Triggers = append([]TriggerRule{}, rules...)
	if err := next.saveLocked(); err != nil {
		return err
	}
	c.Triggers = next.Triggers
	return nil
}
