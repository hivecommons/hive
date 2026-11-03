package github

import (
	"encoding/json"
	"testing"
)

func TestRequiredCheckBindingPresenceAndExplicitAnyApp(t *testing.T) {
	for _, tc := range []struct {
		payload string
		known   bool
	}{
		{`{"context":"build","app_id":11}`, true},
		{`{"context":"build","app_id":null}`, true},
		{`{"context":"build","app_id":-1}`, true},
		{`{"context":"build"}`, false},
		{`{"context":"build","app_id":"11"}`, false},
	} {
		var check requiredCheckWire
		if err := json.Unmarshal([]byte(tc.payload), &check); err != nil {
			t.Fatal(err)
		}
		bindings := bindingsFromWire([]string{"build"}, []requiredCheckWire{check})
		if len(bindings) != 1 || bindings[0].AppKnown != tc.known ||
			observedRequiredBindings(map[string]bool{"build": true}, bindings) != tc.known {
			t.Fatalf("%s produced %+v", tc.payload, bindings)
		}
	}
	if bindings := bindingsFromWire([]string{"build"}, nil); observedRequiredBindings(map[string]bool{"build": true}, bindings) {
		t.Fatal("name-only fallback claimed a known App constraint")
	}
	for _, id := range []string{"11", "null", "-1"} {
		var payload any
		if err := json.Unmarshal([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build","integration_id":`+id+`}]}}]`), &payload); err != nil {
			t.Fatal(err)
		}
		bindings := requiredBindingsFromRules(payload)
		if len(bindings) != 1 || !bindings[0].AppKnown {
			t.Fatalf("rules lost an explicit App constraint: %+v", bindings)
		}
	}
	for _, field := range []string{"", `,"integration_id":"11"`, `,"integration_id":1.5`, `,"integration_id":9007199254740994`} {
		var payload any
		if err := json.Unmarshal([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build"`+field+`}]}}]`), &payload); err != nil {
			t.Fatal(err)
		}
		if bindings := requiredBindingsFromRules(payload); observedRequiredBindings(map[string]bool{"build": true}, bindings) {
			t.Fatalf("missing/malformed rules binding claimed known policy: %s", field)
		}
	}
}
