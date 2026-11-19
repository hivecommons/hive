package automerge

import "testing"

// NativeQueueOptedIn is what the merge relay asks before raising the
// merge-queue-required banner: the policy must be on, the repo and the PR's
// base allow-listed and the transport able to enqueue.
func TestNativeQueueOptedInForRelay(t *testing.T) {
	c, _, _, _ := nativeQueueEngine()
	enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, Repos: map[string]bool{"acme/widget": true}})
	if !c.NativeQueueOptedIn("acme/widget", "main") || !c.NativeQueueOptedIn("Acme/Widget", "main") {
		t.Fatal("allow-listed repo not reported opted in")
	}
	if c.NativeQueueOptedIn("acme/other", "main") {
		t.Fatal("repo outside the allow-list reported opted in")
	}
	enableNativeQueue(c, NativeMergeQueuePolicy{})
	if c.NativeQueueOptedIn("acme/widget", "main") {
		t.Fatal("policy off but repo reported opted in")
	}
	var nilEngine *Engine
	if nilEngine.NativeQueueOptedIn("acme/widget", "main") {
		t.Fatal("nil engine reported opted in")
	}
}

// A Bases allow-list that leaves the PR's base out means the lanes will not
// enqueue that PR (nativeQueueEnabled checks baseAllowed), so the relay must
// keep the banner for it.
func TestNativeQueueOptedInHonoursBases(t *testing.T) {
	c, _, _, _ := nativeQueueEngine()
	enableNativeQueue(c, NativeMergeQueuePolicy{
		Enabled: true,
		Repos:   map[string]bool{"acme/widget": true},
		Bases:   map[string]bool{"acme/widget#main": true},
	})
	if !c.NativeQueueOptedIn("acme/widget", "main") {
		t.Fatal("allow-listed base not reported opted in")
	}
	if c.NativeQueueOptedIn("acme/widget", "release") {
		t.Fatal("base outside the Bases allow-list reported opted in")
	}
}
