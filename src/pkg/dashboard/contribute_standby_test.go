package dashboard

import "testing"

func TestServerCapabilitiesAdvertiseStandbyV1(t *testing.T) {
	for _, cap := range serverCapabilities() {
		if cap == capStandbyV1 {
			return
		}
	}
	t.Fatalf("serverCapabilities() = %v, want %q", serverCapabilities(), capStandbyV1)
}

func TestNormalizeStandbyLanes(t *testing.T) {
	got := normalizeStandbyLanes([]string{" Quality ", "quality", "", "CI-Maintainer"})
	want := []string{"quality", "ci-maintainer"}
	if len(got) != len(want) {
		t.Fatalf("lanes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lanes = %v, want %v", got, want)
		}
	}
	if fallback := normalizeStandbyLanes(nil); len(fallback) != 1 || fallback[0] != "all" {
		t.Fatalf("empty lanes = %v, want [all]", fallback)
	}
}

func TestStandbyAckOmitsFloorOnRejectedLane(t *testing.T) {
	msg := WSMessage{Type: "standby_ack", Standby: &WSStandby{Rejected: []WSStandbyLane{{Lane: "quality", Reason: "below_floor"}}}}
	if msg.Standby.Rejected[0].Floor != "" {
		t.Fatalf("rejected standby lane carried floor %q", msg.Standby.Rejected[0].Floor)
	}
}
