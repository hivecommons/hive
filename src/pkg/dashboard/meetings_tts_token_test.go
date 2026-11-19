package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMeetingsSettingsTTSToken pins the write-only TTS token
// (hivecommons/hive#18393): the API reports only tts_token_set, the token
// is sealed into meetings.tts_token_file, and an empty string clears it.
func TestMeetingsSettingsTTSToken(t *testing.T) {
	s, deps := apiServer(t)
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "tts")
	deps.Config.Meetings.TTSTokenFile = tokFile
	deps.Config.Meetings.TTSURL = "http://vexa-tts.meetings.svc.cluster.local:8880"
	var got MeetingsSettings
	rec := doOwnerGet(s, "/api/meetings/settings")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	if got.TTSTokenSet || got.Effective["meetings.tts_url"] != "http://vexa-tts.meetings.svc.cluster.local:8880" {
		t.Fatalf("defaults %+v", got)
	}
	const secret = "tts-secret-value"
	rec = meetingsOwnerPost(s, "/api/meetings/settings", `{"tts_token":"`+secret+`"}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("post = %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.TTSTokenSet || !got.RestartRequired {
		t.Fatalf("after post %+v", got)
	}
	if fi, err := os.Stat(tokFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, %v", fi, err)
	}
	if v, err := deps.Config.Meetings.TTSToken(context.Background()); err != nil || v != secret {
		t.Fatalf("stored token = %q, %v", v, err)
	}
	if rec := doOwnerGet(s, "/api/meetings/settings"); strings.Contains(rec.Body.String(), secret) {
		t.Fatal("GET must never echo the TTS token")
	}
	rec = meetingsOwnerPost(s, "/api/meetings/settings", `{"tts_token":""}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.TTSTokenSet {
		t.Fatal("an empty TTS token clears it")
	}
	if _, err := os.Stat(tokFile); !os.IsNotExist(err) {
		t.Fatalf("cleared token file still present: %v", err)
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps.Config.Meetings.TTSTokenFile = filepath.Join(blocker, "t")
	if rec := meetingsOwnerPost(s, "/api/meetings/settings", `{"tts_token":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("unwritable token = %d", rec.Code)
	}
}

// TestSettingsMeetingsTTSTokenField pins the Settings → Meetings TTS token
// field: write-only, after the transcription token, sent only when filled,
// and the Discord hint names the in-cluster TTS URL.
func TestSettingsMeetingsTTSTokenField(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{
		`TTS token ' + (s.tts_token_set ? '(set)'`,
		`type="password" data-meetings-setting="tts_token" placeholder="leave blank to keep" autocomplete="new-password"`,
		`meetingsSetting('tts_token')`,
		`if (tts) body.tts_token = tts;`,
		`http://vexa-tts.meetings.svc.cluster.local:8880`,
		"yamlHint('meetings.tts_token_file')",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Settings → Meetings TTS token wiring missing %q", want)
		}
	}
	tx := strings.Index(page, `data-meetings-setting="transcribe_token"`)
	tts := strings.Index(page, `data-meetings-setting="tts_token"`)
	hint := strings.Index(page, `data-meetings-setting="discord_hint"`)
	if tx < 0 || tts < tx || hint < tts {
		t.Fatalf("field order transcribe=%d tts=%d hint=%d", tx, tts, hint)
	}
	if strings.Contains(page, "s.tts_token +") || strings.Contains(page, "s.tts_token)") {
		t.Fatal("the TTS token must never be rendered back into the form")
	}
}
