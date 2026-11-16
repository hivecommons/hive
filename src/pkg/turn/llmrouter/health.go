package llmrouter

import (
	"errors"
	"time"

	"github.com/hivecommons/hive/crux/inferencehealth"
)

// HealthHook returns an attempt hook that records provider faults in the same
// crux/inferencehealth store the gateway proxy writes, so a throttled or
// broken free-tier provider appears in the existing gateway-health surface
// under its gateway name. A served attempt clears the provider's fault.
// Skipped attempts are local decisions (cooldown, cap, no key) and are not
// recorded.
func HealthHook(store *inferencehealth.Store, now func() time.Time) func(Attempt) {
	if now == nil {
		now = time.Now
	}
	return func(a Attempt) {
		if store == nil {
			return
		}
		switch a.Outcome {
		case OutcomeServed:
			store.Clear(a.Provider)
		case OutcomeSkipped:
		default:
			if a.Status > 0 {
				store.RecordHTTPError(a.Provider, a.Status, a.Detail, now())
				return
			}
			store.RecordError(a.Provider, errors.New(a.Detail), now())
		}
	}
}
