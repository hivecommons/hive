package github

import (
	"encoding/json"
	"testing"
	"time"
)

// The batch-built PR detail is what recordReviewEvidenceCI starts an
// evidence bundle from; without base_sha the bundle fails validation and is
// dropped ("evidence: base_sha is required").
func TestGraphQLPRBatchNodeCarriesBaseSHA(t *testing.T) {
	var n graphQLPRBatchNode
	raw := `{"number":9,"headRefOid":"98489e7ba141fc60c30c9aaf47774dc42f9b1c76","baseRefOid":"2afe73bf5ec57745a8f1f7ee8df0b1c86a74e6e9","updatedAt":"2026-10-20T20:00:00Z","baseRefName":"v5","author":{"login":"mendezr"},"title":"x"}`
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatal(err)
	}
	c := &Client{logger: testLogger()}
	c.storeGraphQLPRBatchNode("o/r", n)
	pr, ok := sharedPRDetailCache.getAny("o/r", 9, time.Hour)
	if !ok || pr == nil {
		t.Fatal("batch node was not stored as PR detail")
	}
	if pr.GetBase().GetSHA() != "2afe73bf5ec57745a8f1f7ee8df0b1c86a74e6e9" || pr.GetBase().GetRef() != "v5" {
		t.Fatalf("base = %+v, want sha and ref from the batch node", pr.GetBase())
	}
}
