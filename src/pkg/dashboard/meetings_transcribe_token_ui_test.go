package dashboard

import (
	"strings"
	"testing"
)

// TestSettingsMeetingsTranscribeTokenField pins the Settings → Meetings
// transcription token field (hivecommons/hive#18353): a write-only password
// input next to the Discord token, sent as transcribe_token only when filled,
// and the hint naming everything Discord needs.
func TestSettingsMeetingsTranscribeTokenField(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{
		`Transcription token ' + (s.transcribe_token_set ? '(set)'`,
		`type="password" data-meetings-setting="transcribe_token" placeholder="leave blank to keep" autocomplete="new-password"`,
		`meetingsSetting('transcribe_token')`,
		`if (tx) body.transcribe_token = tx;`,
		`data-meetings-setting="discord_hint"`,
		`http://vexa-transcription.meetings.svc.cluster.local:8000`,
		"yamlHint('meetings.transcribe_token_file')",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Settings → Meetings transcription token wiring missing %q", want)
		}
	}
	// The field must sit after the Discord bot token field, never echo a
	// value back, and the hint must follow both.
	discord := strings.Index(page, `data-meetings-setting="discord_token"`)
	tx := strings.Index(page, `data-meetings-setting="transcribe_token"`)
	hint := strings.Index(page, `data-meetings-setting="discord_hint"`)
	if discord < 0 || tx < discord || hint < tx {
		t.Fatalf("field order discord=%d transcribe=%d hint=%d", discord, tx, hint)
	}
	if strings.Contains(page, "s.transcribe_token +") || strings.Contains(page, "s.transcribe_token)") {
		t.Fatal("the transcription token must never be rendered back into the form")
	}
}
