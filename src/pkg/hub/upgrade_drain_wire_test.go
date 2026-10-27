package hub

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// TestSpokeUpgradeDrainWireCompat pins that the spoke's drain report decodes
// into the hub's mirror field-for-field, and that a spoke not draining sends
// nothing — the hub treats a missing report as "no hold".
func TestSpokeUpgradeDrainWireCompat(t *testing.T) {
	in := spoke.HeartbeatPayload{UpgradeDrain: &spoke.UpgradeDrainReport{
		Target: "4f8be0b", InFlight: []string{"brainstorm"}, WaitedS: 600, RemainingS: 2100,
	}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out HeartbeatPayload
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	want := &UpgradeDrainReport{Target: "4f8be0b", InFlight: []string{"brainstorm"}, WaitedS: 600, RemainingS: 2100}
	if !reflect.DeepEqual(out.UpgradeDrain, want) {
		t.Fatalf("decoded %+v, want %+v", out.UpgradeDrain, want)
	}

	raw, _ = json.Marshal(spoke.HeartbeatPayload{})
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["upgrade_drain"]; present {
		t.Fatal("a spoke that is not draining must omit upgrade_drain")
	}
}
