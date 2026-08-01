package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// alertsSourceFile is the file declaring both the AlertType* constants and the
// knownAlertTypes registry. Parsing the source rather than hand-listing the
// constants is the whole point: a hand-written list is exactly the thing that
// drifted and produced this bug in the first place.
const alertsSourceFile = "alerts.go"

// alertTypeConstPrefix is the naming convention every alert type identifier
// follows. The test discovers types by this prefix, so a newly added one is
// picked up with no test edit at all.
const alertTypeConstPrefix = "AlertType"

// TestKnownAlertTypesCoversEveryAlertType is the guard for the ack endpoint.
//
// handleAlertAck rejects any type not in knownAlertTypes with HTTP 400. When a
// type is declared but never registered, the alert renders in the "Attention
// needed" panel with a working-looking Acknowledge button that can never
// succeed — the alert simply stays put. AlertTypeFailedUpgrade shipped that way
// in #2308.
//
// This parses alerts.go for every AlertType* constant and asserts each one is
// ackable, so adding a constant without registering it fails here rather than
// silently in production.
func TestKnownAlertTypesCoversEveryAlertType(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, alertsSourceFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", alertsSourceFile, err)
	}

	// declared maps constant name -> its string value, for every AlertType*.
	declared := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !isAlertTypeConstName(name.Name) || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s = %s: %v", name.Name, lit.Value, err)
				}
				declared[name.Name] = val
			}
		}
	}

	if len(declared) == 0 {
		t.Fatalf("found no %s* constants in %s — the parser or the naming convention changed", alertTypeConstPrefix, alertsSourceFile)
	}

	for name, val := range declared {
		if !isKnownAlertType(val) {
			t.Errorf("%s (%q) is not in knownAlertTypes: its ack endpoint returns 400 and the alert can never be dismissed", name, val)
		}
	}

	// The reverse direction: a registry entry with no constant behind it is a
	// typo that silently accepts a key nothing ever emits.
	values := map[string]bool{}
	for _, v := range declared {
		values[v] = true
	}
	for registered := range knownAlertTypes {
		if !values[registered] {
			t.Errorf("knownAlertTypes has %q, which matches no %s* constant", registered, alertTypeConstPrefix)
		}
	}
}

// isAlertTypeConstName reports whether an identifier follows the AlertType*
// convention. Requires at least one character after the prefix so the bare
// prefix, if it ever became a real identifier, is not mistaken for a type.
func isAlertTypeConstName(name string) bool {
	return len(name) > len(alertTypeConstPrefix) && name[:len(alertTypeConstPrefix)] == alertTypeConstPrefix
}

// TestAckFailedUpgradeSucceedsAndSurvivesRestart is the end-to-end regression
// for the reported bug: "acknowledging an alert does not remove it from the
// alert window."
//
// Before AlertTypeFailedUpgrade was registered in knownAlertTypes, this ack
// returned 400 and the alert stayed in the panel. The second half restarts the
// hub from the on-disk ack file to prove the ack is not merely in-memory —
// an ack that evaporates on restart looks like it worked and then regresses.
func TestAckFailedUpgradeSucceedsAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	orig := alertAcksPath
	alertAcksPath = filepath.Join(dir, "acks.json")
	defer func() { alertAcksPath = orig }()

	// A spoke that reported an upgrade it could not complete.
	var entry MyHiveEntry
	entry.ID = "hive-failed-upgrade"
	entry.UpgradeFailed = true
	entry.UpgradeTarget = "v2.3.0"
	entry.UpgradeError = "image pull failed"

	s := newTestAlertServer()
	summary := s.fleetAlerts([]MyHiveEntry{entry})
	if !hasUnackedAlertOfType(summary.Alerts, AlertTypeFailedUpgrade) {
		t.Fatalf("expected a live %s alert to acknowledge, got %+v", AlertTypeFailedUpgrade, summary.Alerts)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/saas/admin/alert-ack",
		strings.NewReader(`{"hiveId":"hive-failed-upgrade","type":"failed-upgrade"}`))
	s.handleAlertAck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("acking %s returned %d, want 200 (body: %s)", AlertTypeFailedUpgrade, rec.Code, rec.Body.String())
	}

	// The alert must now be marked acknowledged, which is what moves it out of
	// the live rows and behind the "N acknowledged" disclosure in the panel.
	after := s.fleetAlerts([]MyHiveEntry{entry})
	if hasUnackedAlertOfType(after.Alerts, AlertTypeFailedUpgrade) {
		t.Errorf("%s is still listed as unacknowledged after a successful ack", AlertTypeFailedUpgrade)
	}

	// Restart: a brand-new server reading the same on-disk file must still see
	// the acknowledgement.
	restarted := newTestAlertServer()
	restarted.loadAlertAcks()
	afterRestart := restarted.fleetAlerts([]MyHiveEntry{entry})
	if hasUnackedAlertOfType(afterRestart.Alerts, AlertTypeFailedUpgrade) {
		t.Errorf("the %s acknowledgement did not survive a hub restart", AlertTypeFailedUpgrade)
	}
}

// hasUnackedAlertOfType reports whether any alert of the given type is still
// live and unacknowledged — i.e. whether the panel would still show it.
func hasUnackedAlertOfType(alerts []Alert, alertType string) bool {
	for _, a := range alerts {
		if a.Type == alertType && !a.Acknowledged {
			return true
		}
	}
	return false
}
