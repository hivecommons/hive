package lanes

import "testing"

const budgetCacheReaders = 8

func TestCompletionBudgetCachePreservesConcurrentValuesAndError(t *testing.T) {
	base, delivery, err := buildCompletionBudget()
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		completionSizes
		err error
	}
	results := make(chan result, budgetCacheReaders)
	for n := 0; n < budgetCacheReaders; n++ {
		go func() {
			base, delivery, err := completionBudget()
			results <- result{completionSizes: completionSizes{base: base, delivery: delivery}, err: err}
		}()
	}
	for n := 0; n < budgetCacheReaders; n++ {
		got := <-results
		if got.err != nil || got.base != base || got.delivery != delivery {
			t.Fatalf("cached budget differs: %+v", got)
		}
	}
}
