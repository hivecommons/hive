package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDiscordRFCRichConfigDefaultsOff(t *testing.T) {
	var d DiscordConfig
	if err := yaml.Unmarshal([]byte("bot_token: tok\nrfc_channel_id: \"123\"\n"), &d); err != nil {
		t.Fatal(err)
	}
	if d.RFCFormat != "" || d.RFCThreads || d.RFCAnnouncer || d.RFCBacklinkComment || d.RFCIncludePrivate {
		t.Fatalf("new RFC keys must default off: %+v", d)
	}
	if !d.RFCFormatValid() || d.RFCFormatMode() != RFCFormatLine {
		t.Fatalf("unset rfc_format must be valid line mode, got %q", d.RFCFormatMode())
	}
	out, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"rfc_format", "rfc_threads", "rfc_announcer", "rfc_backlink_comment", "rfc_include_private"} {
		if strings.Contains(string(out), key) {
			t.Fatalf("zero %s must be omitted:\n%s", key, out)
		}
	}
}

func TestDiscordRFCRichConfigParses(t *testing.T) {
	src := `bot_token: tok
rfc_channel_id: "123"
rfc_format: Card
rfc_threads: true
rfc_announcer: true
rfc_backlink_comment: true
rfc_include_private: true
`
	var d DiscordConfig
	if err := yaml.Unmarshal([]byte(src), &d); err != nil {
		t.Fatal(err)
	}
	if !d.RFCThreads || !d.RFCAnnouncer || !d.RFCBacklinkComment || !d.RFCIncludePrivate {
		t.Fatalf("keys not parsed: %+v", d)
	}
	if !d.RFCFormatValid() || d.RFCFormatMode() != RFCFormatCard {
		t.Fatalf("rfc_format Card = %q valid=%v", d.RFCFormatMode(), d.RFCFormatValid())
	}
	if (DiscordConfig{RFCFormat: " line "}).RFCFormatMode() != RFCFormatLine {
		t.Fatal("line must stay line")
	}
}

func TestDiscordRFCFormatValidation(t *testing.T) {
	if (DiscordConfig{RFCFormat: "embed"}).RFCFormatValid() {
		t.Fatal("unknown rfc_format must be invalid")
	}
	cfg := validatableConfig(nil)
	cfg.Notifications.Discord = &DiscordConfig{RFCFormat: "embed"}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "rfc_format") {
		t.Fatalf("Validate() = %v, want an rfc_format error", err)
	}
	for _, ok := range []string{"", "line", "card"} {
		cfg.Notifications.Discord = &DiscordConfig{RFCFormat: ok}
		if err := cfg.ValidateWithOptions(ValidateOptions{}); err != nil && strings.Contains(err.Error(), "rfc_format") {
			t.Fatalf("rfc_format %q rejected: %v", ok, err)
		}
	}
}
