package crux

import "testing"

// singleSlotRuntime is a runtime with no fan-out registry (the sidecar
// client's shape): only SetKickObserver.
type singleSlotRuntime struct {
	Runtime
	installed func(agentName, event, detail string)
	calls     int
}

func (r *singleSlotRuntime) SetKickObserver(fn func(agentName, event, detail string)) {
	r.installed = fn
	r.calls++
}

func TestAddLegacyKickObserver_FallsBackToSingleSlot(t *testing.T) {
	rt := &singleSlotRuntime{}
	var got []string
	remove := AddLegacyKickObserver(rt, func(_, event, _ string) { got = append(got, event) })
	if rt.calls != 1 || rt.installed == nil {
		t.Fatalf("SetKickObserver calls = %d, installed = %v", rt.calls, rt.installed != nil)
	}
	rt.installed("scanner", KickEventDelivered, "governor")
	if len(got) != 1 || got[0] != KickEventDelivered {
		t.Fatalf("observer got %v", got)
	}
	remove()
	if rt.calls != 1 {
		t.Fatal("the remover must not clear the single slot another consumer may own")
	}
}

func TestAddLegacyKickObserver_NilSafe(t *testing.T) {
	AddLegacyKickObserver(nil, func(string, string, string) {})()
	rt := &singleSlotRuntime{}
	AddLegacyKickObserver(rt, nil)()
	if rt.calls != 0 {
		t.Fatal("a nil observer must not touch the runtime")
	}
}
