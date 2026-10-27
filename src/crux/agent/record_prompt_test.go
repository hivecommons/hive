package agent

import (
	"sync"
	"testing"
)

// recordedPrompt is one invocation captured by the test callback.
type recordedPrompt struct {
	agent   string
	trigger string
	prompt  string
}

// TestRecordPromptCallbackRoundTrip pins the wiring contract: once a callback
// is set, recordPrompt forwards the agent, trigger and full prompt text to it
// verbatim.
func TestRecordPromptCallbackRoundTrip(t *testing.T) {
	m := testManager(acmmLevelPushCapable)

	var got []recordedPrompt
	m.SetRecordPromptCallback(func(agent, trigger, prompt string) {
		got = append(got, recordedPrompt{agent, trigger, prompt})
	})

	m.recordPrompt("quality", "kick", "review the open PRs")

	if len(got) != 1 {
		t.Fatalf("callback fired %d times, want 1", len(got))
	}
	want := recordedPrompt{"quality", "kick", "review the open PRs"}
	if got[0] != want {
		t.Errorf("callback received %+v, want %+v", got[0], want)
	}
}

// TestRecordPromptWithoutCallbackIsNoOp checks the unwired case: a manager that
// never had a callback set must swallow the call rather than panic on a nil
// dereference. Every kick path calls recordPrompt unconditionally.
func TestRecordPromptWithoutCallbackIsNoOp(t *testing.T) {
	m := testManager(acmmLevelPushCapable)

	// Must not panic.
	m.recordPrompt("quality", "kick", "some prompt")
}

// TestSetRecordPromptCallbackNilClears covers the documented "a nil fn clears
// it" rule: after clearing, a previously wired callback must stop firing.
func TestSetRecordPromptCallbackNilClears(t *testing.T) {
	m := testManager(acmmLevelPushCapable)

	calls := 0
	m.SetRecordPromptCallback(func(agent, trigger, prompt string) { calls++ })
	m.recordPrompt("quality", "kick", "first")
	if calls != 1 {
		t.Fatalf("precondition: callback fired %d times, want 1", calls)
	}

	m.SetRecordPromptCallback(nil)
	m.recordPrompt("quality", "kick", "second")

	if calls != 1 {
		t.Errorf("callback fired %d times after being cleared, want it to stay at 1", calls)
	}
}

// TestSetRecordPromptCallbackReplaces checks a second Set replaces rather than
// chains: only the most recently wired callback receives the prompt.
func TestSetRecordPromptCallbackReplaces(t *testing.T) {
	m := testManager(acmmLevelPushCapable)

	firstCalls, secondCalls := 0, 0
	m.SetRecordPromptCallback(func(agent, trigger, prompt string) { firstCalls++ })
	m.SetRecordPromptCallback(func(agent, trigger, prompt string) { secondCalls++ })

	m.recordPrompt("quality", "kick", "prompt")

	if firstCalls != 0 {
		t.Errorf("the replaced callback fired %d times, want 0", firstCalls)
	}
	if secondCalls != 1 {
		t.Errorf("the current callback fired %d times, want 1", secondCalls)
	}
}

// TestRecordPromptSafeUnderManagerLock is the reason the callback lives in an
// atomic rather than behind m.mu: recordPrompt is documented as safe to call
// with m.mu already held, so it must not re-acquire it. If it did, this would
// deadlock rather than fail.
func TestRecordPromptSafeUnderManagerLock(t *testing.T) {
	m := testManager(acmmLevelPushCapable)

	fired := false
	m.SetRecordPromptCallback(func(agent, trigger, prompt string) { fired = true })

	m.mu.Lock()
	m.recordPrompt("quality", "kick", "prompt while holding the lock")
	m.mu.Unlock()

	if !fired {
		t.Error("callback did not fire when recordPrompt was called under m.mu")
	}
}

// TestRecordPromptConcurrent exercises the atomic under the race detector:
// concurrent Set and recordPrompt calls must not race. Run with -race in CI.
func TestRecordPromptConcurrent(t *testing.T) {
	// concurrentIterations is enough to interleave setters and firers without
	// making the test slow.
	const concurrentIterations = 100

	m := testManager(acmmLevelPushCapable)
	var mu sync.Mutex
	calls := 0

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < concurrentIterations; i++ {
			m.SetRecordPromptCallback(func(agent, trigger, prompt string) {
				mu.Lock()
				calls++
				mu.Unlock()
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < concurrentIterations; i++ {
			m.recordPrompt("quality", "kick", "prompt")
		}
	}()
	wg.Wait()

	// The exact count depends on interleaving; the contract under test is that
	// neither goroutine panics or races.
	mu.Lock()
	defer mu.Unlock()
	if calls > concurrentIterations {
		t.Errorf("callback fired %d times, want at most %d", calls, concurrentIterations)
	}
}
