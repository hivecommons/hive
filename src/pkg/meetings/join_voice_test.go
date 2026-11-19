package meetings

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestJoinArgsVoiceID pins meeting.join's optional voice_id (quorum
// v0.2.0): sent when set, omitted when empty.
func TestJoinArgsVoiceID(t *testing.T) {
	b, _ := json.Marshal(JoinArgs{URL: "https://meet.google.com/abc", DisplayName: "Betty", Voice: true, VoiceID: "af_heart"})
	if !strings.Contains(string(b), `"voice_id":"af_heart"`) {
		t.Fatalf("join args %s", b)
	}
	b, _ = json.Marshal(JoinArgs{DisplayName: "Betty"})
	if strings.Contains(string(b), "voice_id") {
		t.Fatalf("empty voice_id must be omitted: %s", b)
	}
}
