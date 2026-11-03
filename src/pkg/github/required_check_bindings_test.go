package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRequiredCheckPolicy404PreservesSDKFallbackSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, message, source          string
		status                         int
		readableBranch, configKnown    bool
		known, empty, expectBranchRead bool
	}{
		{"generic not found reads branch", "Not Found", "branch", http.StatusNotFound, true, false, true, false, true},
		{"empty not found reads branch", "", "branch", http.StatusNotFound, true, false, true, false, true},
		{"explicit unprotected is authoritative", "Branch not protected", "protection-unprotected", http.StatusNotFound, true, false, true, true, false},
		{"generic not found can use config", "Not Found", "config", http.StatusNotFound, false, true, true, false, true},
		{"generic not found without evidence is unknown", "Not Found", "none", http.StatusNotFound, false, false, false, true, true},
		{"denied without evidence is unknown", "Resource not accessible by integration", "none", http.StatusForbidden, false, false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var branchReads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
					w.WriteHeader(tc.status)
					fmt.Fprintf(w, `{"message":%q}`, tc.message)
				case r.URL.Path == "/repos/org/repo/branches/v5":
					branchReads.Add(1)
					if tc.readableBranch {
						fmt.Fprint(w, `{"protected":true,"protection":{"required_status_checks":{"contexts":["build"],"checks":[{"context":"build","app_id":11}]}}}`)
					} else {
						http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
					}
				default:
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := NewClientForTest(srv.URL, "org", []string{"repo"}, nil)
			required, known, fromConfig, fallback, source, bindings := RequiredStatusCheckPolicy(
				context.Background(), c.client, "org", "repo", "v5", map[string]bool{"configured": true}, tc.configKnown)
			if known != tc.known || source != tc.source || (len(required) == 0) != tc.empty ||
				(branchReads.Load() > 0) != tc.expectBranchRead || fromConfig != (tc.source == "config") ||
				fallback != (tc.source == "config") {
				t.Fatalf("required=%v known=%v source=%q config=%v fallback=%v branch reads=%d", required, known, source, fromConfig, fallback, branchReads.Load())
			}
			if source == "branch" && (!required["build"] || len(bindings) != 1 ||
				!bindings[0].AppKnown || bindings[0].AppID == nil || *bindings[0].AppID != 11) {
				t.Fatalf("readable branch fallback lost the actual required producer: %v %+v", required, bindings)
			}
		})
	}
}

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
