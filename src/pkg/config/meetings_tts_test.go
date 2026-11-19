package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMeetingsTTS pins the Discord speech settings (hivecommons/hive#18393):
// meetings.tts_url and the write-only, keystore-sealed TTS token, each with
// an env override.
func TestMeetingsTTS(t *testing.T) {
	ctx := context.Background()
	var m MeetingsConfig
	if m.TTSURLOrEmpty() != "" || m.TTSTokenFileOrDefault() != DefaultMeetingsTTSTokenFile {
		t.Fatalf("defaults %q %q", m.TTSURLOrEmpty(), m.TTSTokenFileOrDefault())
	}
	tok := filepath.Join(t.TempDir(), "secrets", "tts")
	m = MeetingsConfig{TTSURL: "http://vexa-tts.meetings.svc.cluster.local:8880/", TTSTokenFile: tok}
	if m.TTSURLOrEmpty() != "http://vexa-tts.meetings.svc.cluster.local:8880" || m.TTSTokenFileOrDefault() != tok {
		t.Fatalf("yaml %q %q", m.TTSURLOrEmpty(), m.TTSTokenFileOrDefault())
	}
	if v, err := m.TTSToken(ctx); err != nil || v != "" {
		t.Fatalf("missing token file: %q, %v", v, err)
	}
	if err := WriteMeetingsSecret(ctx, tok, " tts-token\n"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(tok); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode: %v, %v", fi, err)
	}
	if v, err := m.TTSToken(ctx); err != nil || v != "tts-token" {
		t.Fatalf("TTSToken = %q, %v", v, err)
	}
	t.Setenv(MeetingsTTSURLEnv, "http://env-tts:8880")
	t.Setenv(MeetingsTTSTokenEnv, "env-token")
	t.Setenv(MeetingsTTSTokenFileEnv, "/env/tts")
	if m.TTSURLOrEmpty() != "http://env-tts:8880" || m.TTSTokenFileOrDefault() != "/env/tts" {
		t.Fatal("env overrides")
	}
	if v, _ := m.TTSToken(ctx); v != "env-token" {
		t.Fatalf("env token must win, got %q", v)
	}
	t.Setenv(MeetingsTTSURLEnv, "")
	bad := MeetingsConfig{Enabled: true, TTSURL: "kokoro"}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "tts_url") {
		t.Fatalf("validate %v", err)
	}
}
