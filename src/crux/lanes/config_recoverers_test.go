package lanes

import (
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api"
)

func TestRecoverersValidationDoesNotDependOnGroupIteration(t *testing.T) {
	repeatedMembers := make([]string, DefaultMaxMembers)
	for i := range repeatedMembers {
		repeatedMembers[i] = "atlas"
	}
	tests := []struct {
		name       string
		groups     map[string][]string
		recoverers map[string][]string
		valid      bool
	}{
		{name: "nil groups and recoverers", valid: true},
		{name: "empty groups and recoverers", groups: map[string][]string{}, recoverers: map[string][]string{}, valid: true},
		{name: "nil groups with otherwise valid recovery identifiers", recoverers: map[string][]string{"workers": {"atlas"}}},
		{name: "empty groups with otherwise valid recovery identifiers", groups: map[string][]string{}, recoverers: map[string][]string{"workers": {"atlas"}}},
		{name: "empty groups with malformed recovery group", groups: map[string][]string{}, recoverers: map[string][]string{"bad.*": {"atlas"}}},
		{name: "empty groups with whitespace recovery group", groups: map[string][]string{}, recoverers: map[string][]string{"workers ": {"atlas"}}},
		{name: "empty groups with whitespace recovery member", groups: map[string][]string{}, recoverers: map[string][]string{"workers": {"atlas "}}},
		{name: "empty groups with empty recovery members", groups: map[string][]string{}, recoverers: map[string][]string{"workers": {}}},
		{name: "empty groups with duplicate recovery members", groups: map[string][]string{}, recoverers: map[string][]string{"workers": {"atlas", "atlas"}}},
		{name: "empty groups with excessive recovery members", groups: map[string][]string{}, recoverers: map[string][]string{"workers": append(append([]string(nil), repeatedMembers...), "atlas")}},
		{name: "empty groups with oversized recovery group", groups: map[string][]string{}, recoverers: map[string][]string{strings.Repeat("a", MaxIDLength+1): {"atlas"}}},
		{name: "registered group without recovery grant", groups: map[string][]string{"workers": {"atlas"}}, valid: true},
		{name: "registered group with empty recovery map", groups: map[string][]string{"workers": {"atlas"}}, recoverers: map[string][]string{}, valid: true},
		{name: "registered group with valid recovery grant", groups: map[string][]string{"workers": {"atlas"}}, recoverers: map[string][]string{"workers": {"atlas"}}, valid: true},
		{name: "duplicate members retain membership semantics", groups: map[string][]string{"workers": {"atlas", "atlas"}}, recoverers: map[string][]string{"workers": {"atlas", "atlas"}}, valid: true},
		{name: "recovery member bound permits existing duplicate semantics", groups: map[string][]string{"workers": {"atlas"}}, recoverers: map[string][]string{"workers": repeatedMembers}, valid: true},
		{name: "bound group rejects whitespace member", groups: map[string][]string{"workers": {"atlas"}}, recoverers: map[string][]string{"workers": {" atlas"}}},
		{name: "bound group rejects excessive recovery members", groups: map[string][]string{"workers": {"atlas"}}, recoverers: map[string][]string{"workers": append(append([]string(nil), repeatedMembers...), "atlas")}},
		{name: "registered principal is not necessarily a group member", groups: map[string][]string{"workers": {"atlas"}}, recoverers: map[string][]string{"workers": {"beacon"}}},
		{name: "recovery group must exist", groups: map[string][]string{"workers": {"atlas"}}, recoverers: map[string][]string{"other": {"atlas"}}},
		{name: "membership in another group grants no recovery", groups: map[string][]string{"workers": {"atlas"}, "other": {"beacon"}}, recoverers: map[string][]string{"workers": {"beacon"}}},
		{name: "unknown principal cannot recover", groups: map[string][]string{"workers": {"atlas"}}, recoverers: map[string][]string{"workers": {"outsider"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig("nats://127.0.0.1:4222")
			cfg.Lanes[0].Groups = tt.groups
			cfg.Lanes[0].Recoverers = tt.recoverers
			cfg.Defaults()
			err := cfg.Validate()
			if tt.valid {
				if err != nil {
					t.Fatal("valid membership configuration rejected", err)
				}
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid recovery capability accepted", err)
			}
		})
	}
}

func TestRecoveryConfigurationDefaultsPreserveEmptyLaneRegistry(t *testing.T) {
	cfg := Config{
		URL:        "nats://127.0.0.1:4222",
		Principals: []Principal{{Project: "atlas", TokenEnv: "ATLAS_TOKEN"}},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal("defaulted empty lane registry rejected", err)
	}
}

func TestRecoveryAuthorizationRemainsProjectLaneAndGroupScoped(t *testing.T) {
	cfg := testConfig("nats://127.0.0.1:4222")
	cfg.Lanes[0].Recoverers = map[string][]string{"workers": {"atlas", "atlas"}}
	cfg.Lanes = append(cfg.Lanes, api.Lane{
		ID: "separate", Owner: "beacon",
		Groups:     map[string][]string{"workers": {"beacon"}},
		Recoverers: map[string][]string{"workers": {"beacon"}},
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal("valid recovery authorization fixture rejected", err)
	}
	b := &Broker{registry: map[string]api.Lane{}}
	for _, lane := range cfg.Lanes {
		b.registry[lane.ID] = lane
	}
	tests := []struct {
		project, lane, group string
		allowed              bool
	}{
		{"atlas", "shared", "workers", true},
		{"beacon", "shared", "workers", false},
		{"atlas", "shared", "other", false},
		{"beacon", "shared", "other", false},
		{"beacon", "separate", "workers", true},
		{"atlas", "separate", "workers", false},
		{"outsider", "shared", "workers", false},
		{"atlas", "unknown", "workers", false},
		{"atlas", "shared", "unknown", false},
	}
	for _, tt := range tests {
		t.Run(tt.project+"/"+tt.lane+"/"+tt.group, func(t *testing.T) {
			_, err := b.authorize(tt.project, tt.lane, "recover", tt.group)
			if tt.allowed {
				if err != nil {
					t.Fatal("authorized recovery rejected", err)
				}
			} else if !errors.Is(err, ErrForbidden) {
				t.Fatal("foreign recovery capability accepted", err)
			}
		})
	}
}
