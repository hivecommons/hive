package spoke

import "sync"

// nonOverlappingHeartbeatCollector bounds a loop to one active invocation of
// this collector, including invocations that outlive collectWithTimeout. Busy
// beats return nil immediately and use sendHeartbeat's stale/minimal fallback.
// The lock belongs to the loop, not to process-global test or payload state.
func nonOverlappingHeartbeatCollector(collect StatusCollector) StatusCollector {
	var active sync.Mutex
	return func() *HeartbeatPayload {
		if !active.TryLock() {
			return nil
		}
		defer active.Unlock() // also release when the timeout helper recovers a panic
		return collect()
	}
}
