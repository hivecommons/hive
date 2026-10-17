package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

func seedNousSelfLedger(deps *Dependencies) {
	deps.Nous.Ledger = []map[string]interface{}{
		{"action": "repo-exp", "id": "repo-1", "ts": "2024-01-01"},
		{"id": "self-1", "source": "hive-self", "hypothesis": "SELF-SOURCE"},
		{"id": "self-2", "scope": "self", "hypothesis": "SELF-SCOPE"},
		{
			"id": "self-3", "hypothesis": "SELF-EXPERIMENT",
			"subject": map[string]interface{}{"kind": "flag", "reference": "x"},
			"cohorts": []interface{}{},
		},
		{"experiment_id": "self-4", "flag": "SELF-FLAG", "enabled": true},
	}
}

func TestNousLedgerExcludesHiveSelfEntries(t *testing.T) {
	s, deps := apiServerWithNous(t)
	seedNousSelfLedger(deps)

	rec := doGet(s, "/api/nous/ledger")
	var got []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["id"] != "repo-1" {
		t.Fatalf("ledger = %v, want only repo-1", got)
	}
}

func TestNousLedgerAllSelfReturnsEmptyArray(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Nous.Ledger = []map[string]interface{}{{"source": "hive-self"}}
	rec := doGet(s, "/api/nous/ledger")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("body = %q, want []", rec.Body.String())
	}
}

func TestNousStatusExcludesHiveSelfRecords(t *testing.T) {
	s, deps := apiServerWithNous(t)
	seedNousSelfLedger(deps)
	deps.Nous.Status["self_flags"] = map[string]interface{}{"flag": "SELF-FLAG"}
	deps.Nous.Status["selfFlagAssignment"] = map[string]interface{}{"flag": "SELF-FLAG"}

	rec := doGet(s, "/api/nous/status")
	body := rec.Body.String()
	for _, leak := range []string{"SELF-SOURCE", "SELF-SCOPE", "SELF-EXPERIMENT", "SELF-FLAG", "self-1", "self-4"} {
		if strings.Contains(body, leak) {
			t.Fatalf("status leaked %q: %s", leak, body)
		}
	}
}
