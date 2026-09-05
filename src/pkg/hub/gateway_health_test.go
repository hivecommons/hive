package hub

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/inferencehealth"
)

func TestSanitizeGatewayHealth_EmptyInput(t *testing.T) {
	if got := sanitizeGatewayHealth(nil); got != nil {
		t.Errorf("sanitizeGatewayHealth(nil) = %+v, want nil", got)
	}
	if got := sanitizeGatewayHealth([]inferencehealth.GatewayStatus{}); got != nil {
		t.Errorf("sanitizeGatewayHealth(empty) = %+v, want nil", got)
	}
}

func TestSanitizeGatewayHealth_FiltersAndNormalizes(t *testing.T) {
	in := []inferencehealth.GatewayStatus{
		// Dropped: blank name / blank class.
		{Name: "  ", ErrorClass: inferencehealth.ClassDNS},
		{Name: "noclass", ErrorClass: "   "},
		// Unknown class collapses to "other".
		{Name: "weird", ErrorClass: "made-up-class", HTTPStatus: 200, LastErrorAt: "2026-01-01T00:00:00Z"},
		// Invalid timestamp is cleared; status is clamped to [0,599].
		{Name: "badtime", ErrorClass: inferencehealth.Class5xx, HTTPStatus: 700, LastErrorAt: "yesterday-ish"},
		{Name: "negstatus", ErrorClass: inferencehealth.ClassConnect, HTTPStatus: -3, LastErrorAt: "2026-01-02T03:04:05Z"},
		// Detail is prose-sanitized: markup and control chars removed.
		{Name: "detail", ErrorClass: inferencehealth.ClassAuth, Detail: "a <script>\x1b[31m`bad`\n stuff"},
	}
	out := sanitizeGatewayHealth(in)
	if len(out) != 4 {
		t.Fatalf("sanitized %d entries (%+v), want 4", len(out), out)
	}
	byName := map[string]inferencehealth.GatewayStatus{}
	for _, st := range out {
		byName[st.Name] = st
	}
	if st := byName["weird"]; st.ErrorClass != inferencehealth.ClassOther {
		t.Errorf("unknown class = %q, want %q", st.ErrorClass, inferencehealth.ClassOther)
	}
	if st := byName["badtime"]; st.LastErrorAt != "" || st.HTTPStatus != 599 {
		t.Errorf("badtime entry = %+v, want cleared timestamp and status clamped to 599", st)
	}
	if st := byName["negstatus"]; st.HTTPStatus != 0 || st.LastErrorAt != "2026-01-02T03:04:05Z" {
		t.Errorf("negstatus entry = %+v, want status 0 and timestamp preserved", st)
	}
	if st := byName["detail"]; strings.ContainsAny(st.Detail, "<>`\x1b\n") {
		t.Errorf("detail not sanitized: %q", st.Detail)
	}
	// Output is sorted by lowercase name.
	for i := 1; i < len(out); i++ {
		if strings.ToLower(out[i-1].Name) > strings.ToLower(out[i].Name) {
			t.Errorf("output not sorted: %q before %q", out[i-1].Name, out[i].Name)
		}
	}
}

func TestSanitizeGatewayHealth_CapsEntries(t *testing.T) {
	in := make([]inferencehealth.GatewayStatus, 0, gatewayHealthMaxEntries+10)
	for i := 0; i < gatewayHealthMaxEntries+10; i++ {
		in = append(in, inferencehealth.GatewayStatus{
			Name:       fmt.Sprintf("gw-%03d", i),
			ErrorClass: inferencehealth.Class5xx,
		})
	}
	if out := sanitizeGatewayHealth(in); len(out) != gatewayHealthMaxEntries {
		t.Errorf("sanitized %d entries, want cap %d", len(out), gatewayHealthMaxEntries)
	}
}

func TestGatewayFaultForBackend(t *testing.T) {
	faults := []inferencehealth.GatewayStatus{
		{Name: "OpenRouter", ErrorClass: inferencehealth.ClassAuth},
		{Name: "classless", ErrorClass: "  "},
	}
	if _, ok := gatewayFaultForBackend(faults, "  "); ok {
		t.Error("blank backend must not match")
	}
	if _, ok := gatewayFaultForBackend(faults, "unknown"); ok {
		t.Error("unknown backend must not match")
	}
	if _, ok := gatewayFaultForBackend(faults, "classless"); ok {
		t.Error("fault with blank error class must not match")
	}
	st, ok := gatewayFaultForBackend(faults, "openrouter")
	if !ok || st.Name != "OpenRouter" {
		t.Errorf("case-insensitive match failed: ok=%v st=%+v", ok, st)
	}
}

func TestMostRecentGatewayFaultForAgents(t *testing.T) {
	faults := []inferencehealth.GatewayStatus{
		{Name: "older", ErrorClass: inferencehealth.Class5xx, LastErrorAt: "2026-01-01T00:00:00Z"},
		{Name: "newer", ErrorClass: inferencehealth.ClassConnect, LastErrorAt: "2026-01-02T00:00:00Z"},
		{Name: "idlegw", ErrorClass: inferencehealth.ClassDNS, LastErrorAt: "2026-01-05T00:00:00Z"},
	}
	agents := []AgentSummary{
		{Name: "", Backend: "older"},        // blank name: skipped
		{Name: "no-backend", Backend: "  "}, // blank backend: skipped
		{Name: "idle", Backend: "idlegw"},   // not active/enabled/running: skipped
		{Name: "sched", Backend: "older", ExpectedActive: true},
		{Name: "live", Backend: "newer", State: "RUNNING"}, // case-insensitive state match
	}
	st, ok := mostRecentGatewayFaultForAgents(faults, agents)
	if !ok {
		t.Fatal("expected a fault for the active agents")
	}
	if st.Name != "newer" {
		t.Errorf("most recent fault = %+v, want the 'newer' gateway", st)
	}

	// The idle agent's gateway fault alone yields no result.
	if _, ok := mostRecentGatewayFaultForAgents(faults, []AgentSummary{{Name: "idle", Backend: "idlegw"}}); ok {
		t.Error("inactive agent must not surface a gateway fault")
	}
	// Enabled alone is sufficient to consider the agent.
	st, ok = mostRecentGatewayFaultForAgents(faults, []AgentSummary{{Name: "en", Backend: "idlegw", Enabled: true}})
	if !ok || st.Name != "idlegw" {
		t.Errorf("enabled agent should match its backend fault, got ok=%v st=%+v", ok, st)
	}
}
