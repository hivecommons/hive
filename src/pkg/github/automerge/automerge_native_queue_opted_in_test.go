package automerge

import "testing"

// NativeQueueOptedIn is what the merge relay asks before raising the
// merge-queue-required banner: the policy must be on, the repo allow-listed
// and the transport able to enqueue.
func TestNativeQueueOptedInForRelay(t *testing.T) {
	c, _, _, _ := nativeQueueEngine()
	enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, Repos: map[string]bool{"acme/widget": true}})
	if !c.NativeQueueOptedIn("acme/widget") || !c.NativeQueueOptedIn("Acme/Widget") {
		t.Fatal("allow-listed repo not reported opted in")
	}
	if c.NativeQueueOptedIn("acme/other") {
		t.Fatal("repo outside the allow-list reported opted in")
	}
	enableNativeQueue(c, NativeMergeQueuePolicy{})
	if c.NativeQueueOptedIn("acme/widget") {
		t.Fatal("policy off but repo reported opted in")
	}
	var nilEngine *Engine
	if nilEngine.NativeQueueOptedIn("acme/widget") {
		t.Fatal("nil engine reported opted in")
	}
}
