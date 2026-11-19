package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func postProtocolHeartbeat(t *testing.T, s *HubServer, body string) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", heartbeatBearer(testHubSecret, "proto-hive"))
	rec := httptest.NewRecorder()
	s.handleHeartbeat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: status %d body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

const protocolHeartbeatBase = `"hive_id":"proto-hive","org":"o","primary_repo":"r","repos":["r"]`

func decodeProtocolAck(t *testing.T, raw map[string]json.RawMessage) HeartbeatProtocolAck {
	t.Helper()
	field, ok := raw["protocol"]
	if !ok {
		t.Fatalf("response has no protocol field: %v", raw)
	}
	var ack HeartbeatProtocolAck
	if err := json.Unmarshal(field, &ack); err != nil {
		t.Fatal(err)
	}
	return ack
}

func TestNegotiateHeartbeatProtocolAcceptedAndIgnored(t *testing.T) {
	ack := negotiateHeartbeatProtocol(HeartbeatProtocol{
		Version:  1,
		Features: []string{"host_aliases", "future_thing", "activity_digest", " activity_digest ", "future_thing"},
	})
	if ack.Version != heartbeatProtocolVersion {
		t.Fatalf("version = %d, want %d", ack.Version, heartbeatProtocolVersion)
	}
	if want := []string{"activity_digest", "host_aliases"}; !reflect.DeepEqual(ack.Accepted, want) {
		t.Errorf("accepted = %v, want %v", ack.Accepted, want)
	}
	if want := []string{"future_thing"}; !reflect.DeepEqual(ack.Ignored, want) {
		t.Errorf("ignored = %v, want %v", ack.Ignored, want)
	}
	if ack.UnsupportedVersion != nil {
		t.Errorf("unsupported_version = %d, want absent", *ack.UnsupportedVersion)
	}
}

func TestNegotiateHeartbeatProtocolUnknownVersion(t *testing.T) {
	for _, v := range []int{0, -3} {
		ack := negotiateHeartbeatProtocol(HeartbeatProtocol{Version: v, Features: []string{"activity_digest", "x"}})
		if ack.UnsupportedVersion == nil || *ack.UnsupportedVersion != v {
			t.Fatalf("version %d: unsupported_version = %v, want %d", v, ack.UnsupportedVersion, v)
		}
		if ack.Version != heartbeatProtocolVersion {
			t.Errorf("version %d: answered version %d, want %d", v, ack.Version, heartbeatProtocolVersion)
		}
		if len(ack.Accepted) != 0 {
			t.Errorf("version %d: accepted = %v, want none", v, ack.Accepted)
		}
		if want := []string{"activity_digest", "x"}; !reflect.DeepEqual(ack.Ignored, want) {
			t.Errorf("version %d: ignored = %v, want %v", v, ack.Ignored, want)
		}
	}

	// A newer spoke is answered with the hub's version and its known features.
	ack := negotiateHeartbeatProtocol(HeartbeatProtocol{Version: 7, Features: []string{"activity_events"}})
	if ack.Version != heartbeatProtocolVersion || ack.UnsupportedVersion != nil {
		t.Fatalf("newer version: got %+v", ack)
	}
	if want := []string{"activity_events"}; !reflect.DeepEqual(ack.Accepted, want) {
		t.Errorf("newer version: accepted = %v, want %v", ack.Accepted, want)
	}
}

func TestNegotiateHeartbeatProtocolBoundsAndMalformed(t *testing.T) {
	features := []string{"", "Bad", "1abc", "has space", strings.Repeat("a", heartbeatProtocolMaxFeatureLen+1)}
	for i := 0; i < heartbeatProtocolMaxFeatures+10; i++ {
		features = append(features, "f_"+itoa3(i))
	}
	ack := negotiateHeartbeatProtocol(HeartbeatProtocol{Version: 1, Features: features})
	if got := len(ack.Accepted) + len(ack.Ignored); got != heartbeatProtocolMaxFeatures {
		t.Fatalf("echoed %d features, want %d", got, heartbeatProtocolMaxFeatures)
	}
	for _, name := range ack.Ignored {
		if !validHeartbeatFeatureName(name) {
			t.Errorf("malformed feature %q echoed back", name)
		}
	}
	if empty := negotiateHeartbeatProtocol(HeartbeatProtocol{Version: 1}); empty.Accepted == nil || empty.Ignored == nil {
		t.Errorf("no features: accepted and ignored must be empty lists, got %+v", empty)
	}
}

func itoa3(i int) string {
	return string([]byte{byte('0' + i/100%10), byte('0' + i/10%10), byte('0' + i%10)})
}

func TestHeartbeatProtocolAbsentFieldUnchanged(t *testing.T) {
	t.Setenv(heartbeatProtocolEnv, "true")
	raw := postProtocolHeartbeat(t, newActivityHub(), "{"+protocolHeartbeatBase+"}")
	if _, ok := raw["protocol"]; ok {
		t.Fatalf("heartbeat without protocol got a protocol answer: %s", raw["protocol"])
	}
}

func TestHeartbeatProtocolFlagOffIgnoresOffer(t *testing.T) {
	t.Setenv(heartbeatProtocolEnv, "")
	raw := postProtocolHeartbeat(t, newActivityHub(),
		"{"+protocolHeartbeatBase+`,"protocol":{"version":1,"features":["activity_digest"]}}`)
	if _, ok := raw["protocol"]; ok {
		t.Fatalf("flag off: protocol answered: %s", raw["protocol"])
	}
}

func TestHeartbeatProtocolHandlerAnswers(t *testing.T) {
	t.Setenv(heartbeatProtocolEnv, "on")
	raw := postProtocolHeartbeat(t, newActivityHub(),
		"{"+protocolHeartbeatBase+`,"protocol":{"version":1,"features":["host_aliases","not_yet"]}}`)
	ack := decodeProtocolAck(t, raw)
	if want := []string{"host_aliases"}; !reflect.DeepEqual(ack.Accepted, want) {
		t.Errorf("accepted = %v, want %v", ack.Accepted, want)
	}
	if want := []string{"not_yet"}; !reflect.DeepEqual(ack.Ignored, want) {
		t.Errorf("ignored = %v, want %v", ack.Ignored, want)
	}
	var ok bool
	if err := json.Unmarshal(raw["ok"], &ok); err != nil || !ok {
		t.Errorf("ok = %s, want true", raw["ok"])
	}
}

func TestHeartbeatProtocolHandlerUnknownVersionStillAccepted(t *testing.T) {
	t.Setenv(heartbeatProtocolEnv, "1")
	raw := postProtocolHeartbeat(t, newActivityHub(),
		"{"+protocolHeartbeatBase+`,"protocol":{"version":0,"features":["activity_digest"]}}`)
	ack := decodeProtocolAck(t, raw)
	if ack.UnsupportedVersion == nil || *ack.UnsupportedVersion != 0 {
		t.Fatalf("unsupported_version = %v, want 0", ack.UnsupportedVersion)
	}
	if len(ack.Accepted) != 0 || !reflect.DeepEqual(ack.Ignored, []string{"activity_digest"}) {
		t.Errorf("got accepted=%v ignored=%v", ack.Accepted, ack.Ignored)
	}
}
