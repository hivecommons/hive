package forgestate

import (
	"strings"
	"testing"
)

func TestIntegrationContextsRequireEveryPhysicalProducerAndSuccess(t *testing.T) {
	makeRecords := func() map[string]IntegrationContextReceipt {
		out := map[string]IntegrationContextReceipt{}
		for i, name := range IntegrationRequiredContexts() {
			out[name] = IntegrationContextReceipt{JobID: int64(i + 1), CheckID: int64(i + 101), AppID: 9, SuiteID: 10, Conclusion: "success"}
		}
		return out
	}
	if !IntegrationContextsPassed(makeRecords(), strings.Repeat("a", 64)) {
		t.Fatal("complete actual success cohort rejected")
	}
	for _, invalid := range []string{"", strings.Repeat("0", 64), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if IntegrationContextsPassed(makeRecords(), invalid) {
			t.Fatalf("invalid metadata digest accepted: %s", invalid)
		}
	}
	for _, mutate := range []func(map[string]IntegrationContextReceipt){
		func(r map[string]IntegrationContextReceipt) { delete(r, "DCO") },
		func(r map[string]IntegrationContextReceipt) { r["fake"] = r["DCO"] },
		func(r map[string]IntegrationContextReceipt) { v := r["DCO"]; v.JobID = 0; r["DCO"] = v },
		func(r map[string]IntegrationContextReceipt) { v := r["DCO"]; v.CheckID = 0; r["DCO"] = v },
		func(r map[string]IntegrationContextReceipt) { v := r["DCO"]; v.AppID = 0; r["DCO"] = v },
		func(r map[string]IntegrationContextReceipt) { v := r["DCO"]; v.SuiteID = 0; r["DCO"] = v },
		func(r map[string]IntegrationContextReceipt) { v := r["DCO"]; v.Conclusion = "skipped"; r["DCO"] = v },
		func(r map[string]IntegrationContextReceipt) { r["DCO"] = r["gate"] },
	} {
		records := makeRecords()
		mutate(records)
		if IntegrationContextsPassed(records, strings.Repeat("a", 64)) {
			t.Fatal("incomplete/false-green actual context accepted")
		}
	}
}
