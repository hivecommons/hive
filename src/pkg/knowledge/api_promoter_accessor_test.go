package knowledge

import "testing"

// Promoter() feeds the scheduled promotion loop the same *Promoter the
// dashboard's manual promote path uses; if it ever returned a different (or
// nil) instance the two paths could diverge silently.
func TestPromoterAccessor(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, apiTestLogger())
	p := api.Promoter()
	if p == nil {
		t.Fatal("Promoter() = nil, want the API's promoter")
	}
	if p != api.promoter {
		t.Fatal("Promoter() must return the same promoter PromoteFact uses")
	}
}
