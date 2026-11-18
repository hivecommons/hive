package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMeetingsConfigDefaults(t *testing.T) {
	var m MeetingsConfig
	if m.IsEnabled() {
		t.Fatal("meetings must default off")
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"binary", m.QuorumBinary(), DefaultMeetingsQuorumBinary},
		{"listen", m.QuorumListen(), DefaultMeetingsQuorumListen},
		{"vexa", m.VexaURL(), ""},
		{"vexa key file", m.VexaAPIKeyFile(), DefaultMeetingsVexaAPIKeyFile},
		{"discord file", m.DiscordTokenFile(), DefaultMeetingsDiscordTokenFile},
		{"transcribe", m.TranscribeURLOrEmpty(), ""},
		{"transcribe token file", m.TranscribeTokenFileOrDefault(), DefaultMeetingsTranscribeTokenFile},
		{"delivery", m.DeliveryOrDefault(), MeetingsDeliveryChat},
		{"voice", m.VoiceOnAddressEnabled(), true},
		{"bot", m.BotNameOrDefault(), "Betty Bee from the hive"},
		{"invite", m.InvitePolicyOrDefault(), MeetingsInvitePolicyOwner},
		{"pool", m.ResearchPoolSizeOrDefault(), DefaultMeetingsResearchPoolSize},
		{"research cap", m.ResearchPerMeetingOrDefault(), DefaultMeetingsResearchPerMeeting},
		{"chat rate", m.ChatPostsPerMinuteOrDefault(), DefaultMeetingsChatPostsPerMinute},
		{"lobby", m.LobbyTimeoutSOrDefault(), DefaultMeetingsLobbyTimeoutS},
		{"retention", m.RetentionDaysOrDefault(), 30},
		{"consent", m.ConsentTextOrDefault(), DefaultMeetingsConsentText},
		{"data", m.DataDirOrDefault(), DefaultMeetingsDataDir},
		{"tokens meeting", m.Budget.MaxTokensPerMeetingOrDefault(), DefaultMeetingsMaxTokensPerMeeting},
		{"tokens hour", m.Budget.MaxTokensPerHourOrDefault(), DefaultMeetingsMaxTokensPerHour},
		{"tokens day", m.Budget.MaxTokensPerDayOrDefault(), DefaultMeetingsMaxTokensPerDay},
		{"intents", m.Budget.MaxIntentsPerMeetingOrDefault(), DefaultMeetingsMaxIntentsPerMeeting},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if !strings.Contains(DefaultMeetingsConsentText, "hive leave") {
		t.Error("the consent notice must tell people how to remove hive")
	}
}

func TestMeetingsConfigYAMLValues(t *testing.T) {
	voice := false
	m := MeetingsConfig{
		Enabled:             true,
		Quorum:              MeetingsQuorumConfig{Binary: "/opt/q.mjs", Listen: "127.0.0.1:9000"},
		Vexa:                MeetingsVexaConfig{URL: "https://vexa.example/", APIKeyFile: "/data/secrets/vk"},
		Discord:             MeetingsDiscordConfig{TokenFile: "/data/secrets/dt"},
		TranscribeURL:       "http://whisper.local/",
		TranscribeTokenFile: "/data/secrets/tt",
		Delivery:            "CHAT",
		VoiceOnAddress:      &voice,
		BotName:             "notes bot",
		InvitePolicy:        "Owner",
		ResearchPoolSize:    3,
		ResearchPerMeeting:  7,
		ChatPostsPerMinute:  2,
		LobbyTimeoutS:       60,
		RetentionDays:       9,
		ConsentText:         "hello",
		DataDir:             "/tmp/m",
		Budget:              MeetingsBudgetConfig{MaxTokensPerMeeting: 10, MaxTokensPerHour: 20, MaxTokensPerDay: 30, MaxIntentsPerMeeting: 40},
	}
	if !m.IsEnabled() || m.QuorumBinary() != "/opt/q.mjs" || m.QuorumListen() != "127.0.0.1:9000" {
		t.Fatalf("quorum values not honoured: %+v", m.Quorum)
	}
	if m.VexaURL() != "https://vexa.example" || m.VexaAPIKeyFile() != "/data/secrets/vk" || m.DiscordTokenFile() != "/data/secrets/dt" {
		t.Fatal("vexa/discord values not honoured")
	}
	if m.TranscribeURLOrEmpty() != "http://whisper.local" || m.DeliveryOrDefault() != "chat" || m.VoiceOnAddressEnabled() {
		t.Fatal("transcribe/delivery/voice not honoured")
	}
	if m.TranscribeTokenFileOrDefault() != "/data/secrets/tt" {
		t.Fatalf("transcribe token file = %q, want /data/secrets/tt", m.TranscribeTokenFileOrDefault())
	}
	if m.BotNameOrDefault() != "notes bot" || m.InvitePolicyOrDefault() != "owner" || m.ConsentTextOrDefault() != "hello" || m.DataDirOrDefault() != "/tmp/m" {
		t.Fatal("string values not honoured")
	}
	if m.ResearchPoolSizeOrDefault() != 3 || m.ResearchPerMeetingOrDefault() != 7 || m.ChatPostsPerMinuteOrDefault() != 2 ||
		m.LobbyTimeoutSOrDefault() != 60 || m.RetentionDaysOrDefault() != 9 {
		t.Fatal("int values not honoured")
	}
	b := m.Budget
	if b.MaxTokensPerMeetingOrDefault() != 10 || b.MaxTokensPerHourOrDefault() != 20 || b.MaxTokensPerDayOrDefault() != 30 || b.MaxIntentsPerMeetingOrDefault() != 40 {
		t.Fatal("budget values not honoured")
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestMeetingsConfigEnvOverrides(t *testing.T) {
	env := map[string]string{
		MeetingsEnabledEnv:             "true",
		MeetingsQuorumBinaryEnv:        "/env/q.mjs",
		MeetingsQuorumListenEnv:        "localhost:7000",
		MeetingsVexaURLEnv:             "https://env.vexa/",
		MeetingsVexaAPIKeyFileEnv:      "/env/vk",
		MeetingsDiscordTokenFileEnv:    "/env/dt",
		MeetingsTranscribeURLEnv:       "https://env.whisper",
		MeetingsTranscribeTokenFileEnv: "/env/tt",
		MeetingsDeliveryEnv:            "chat",
		MeetingsVoiceOnAddressEnv:      "off",
		MeetingsBotNameEnv:             "env bot",
		MeetingsInvitePolicyEnv:        "owner",
		MeetingsResearchPoolSizeEnv:    "4",
		MeetingsResearchPerMeetingEnv:  "8",
		MeetingsChatPostsPerMinuteEnv:  "3",
		MeetingsLobbyTimeoutEnv:        "30",
		MeetingsRetentionDaysEnv:       "5",
		MeetingsConsentTextEnv:         "env consent",
		MeetingsDataDirEnv:             "/env/data",
		MeetingsTokensPerMeetingEnv:    "11",
		MeetingsTokensPerHourEnv:       "12",
		MeetingsTokensPerDayEnv:        "13",
		MeetingsIntentsPerMeetingEnv:   "14",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	m := MeetingsConfig{ResearchPoolSize: 9, BotName: "yaml bot", TranscribeTokenFile: "/yaml/tt", Budget: MeetingsBudgetConfig{MaxTokensPerDay: 99}}
	if got := m.TranscribeTokenFileOrDefault(); got != "/env/tt" {
		t.Fatalf("transcribe token file env override = %q, want /env/tt", got)
	}
	if !m.IsEnabled() || m.QuorumBinary() != "/env/q.mjs" || m.QuorumListen() != "localhost:7000" ||
		m.VexaURL() != "https://env.vexa" || m.VexaAPIKeyFile() != "/env/vk" || m.DiscordTokenFile() != "/env/dt" ||
		m.TranscribeURLOrEmpty() != "https://env.whisper" || m.VoiceOnAddressEnabled() || m.BotNameOrDefault() != "env bot" ||
		m.ConsentTextOrDefault() != "env consent" || m.DataDirOrDefault() != "/env/data" {
		t.Fatal("string env overrides not honoured")
	}
	if m.ResearchPoolSizeOrDefault() != 4 || m.ResearchPerMeetingOrDefault() != 8 || m.ChatPostsPerMinuteOrDefault() != 3 ||
		m.LobbyTimeoutSOrDefault() != 30 || m.RetentionDaysOrDefault() != 5 {
		t.Fatal("int env overrides not honoured")
	}
	b := m.Budget
	if b.MaxTokensPerMeetingOrDefault() != 11 || b.MaxTokensPerHourOrDefault() != 12 || b.MaxTokensPerDayOrDefault() != 13 || b.MaxIntentsPerMeetingOrDefault() != 14 {
		t.Fatal("budget env overrides not honoured")
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	t.Setenv(MeetingsEnabledEnv, "0")
	if (MeetingsConfig{Enabled: true}).IsEnabled() {
		t.Fatal("HIVE_MEETINGS_ENABLED=0 must win over meetings.enabled")
	}
}

func TestMeetingsConfigInvalidIntEnvFallsBack(t *testing.T) {
	t.Setenv(MeetingsResearchPoolSizeEnv, "lots")
	t.Setenv(MeetingsRetentionDaysEnv, "-3")
	m := MeetingsConfig{ResearchPoolSize: 6}
	if got := m.ResearchPoolSizeOrDefault(); got != 6 {
		t.Fatalf("pool = %d, want the yaml value 6", got)
	}
	if got := m.RetentionDaysOrDefault(); got != DefaultMeetingsRetentionDays {
		t.Fatalf("retention = %d, want the default", got)
	}
}

func TestMeetingsConfigValidate(t *testing.T) {
	long := strings.Repeat("x", meetingsMaxConsentChars+1)
	cases := []struct {
		name string
		m    MeetingsConfig
		want string
	}{
		{"non-loopback listen", MeetingsConfig{Quorum: MeetingsQuorumConfig{Listen: "0.0.0.0:8765"}}, "loopback"},
		{"hostname listen", MeetingsConfig{Quorum: MeetingsQuorumConfig{Listen: "quorum:8765"}}, "loopback"},
		{"no port", MeetingsConfig{Quorum: MeetingsQuorumConfig{Listen: "127.0.0.1"}}, "host:port"},
		{"bad port", MeetingsConfig{Quorum: MeetingsQuorumConfig{Listen: "127.0.0.1:99999"}}, "invalid port"},
		{"vexa scheme", MeetingsConfig{Vexa: MeetingsVexaConfig{URL: "ftp://vexa"}}, "http(s)"},
		{"vexa parse", MeetingsConfig{Vexa: MeetingsVexaConfig{URL: "http://[::1"}}, "not a URL"},
		{"vexa creds", MeetingsConfig{Vexa: MeetingsVexaConfig{URL: "https://u:p@vexa"}}, "credentials"},
		{"transcribe", MeetingsConfig{TranscribeURL: "whisper"}, "transcribe_url"},
		{"delivery", MeetingsConfig{Delivery: "email"}, "delivery"},
		{"invite", MeetingsConfig{InvitePolicy: "anyone"}, "invite_policy"},
		{"bot name", MeetingsConfig{BotName: strings.Repeat("b", meetingsMaxBotNameChars+1)}, "bot_name"},
		{"consent", MeetingsConfig{ConsentText: long}, "consent_text"},
		{"negative", MeetingsConfig{RetentionDays: -1}, "retention_days"},
		{"meeting over day", MeetingsConfig{Budget: MeetingsBudgetConfig{MaxTokensPerMeeting: 5, MaxTokensPerDay: 4, MaxTokensPerHour: 1}}, "max_tokens_per_meeting"},
		{"hour over day", MeetingsConfig{Budget: MeetingsBudgetConfig{MaxTokensPerMeeting: 1, MaxTokensPerDay: 4, MaxTokensPerHour: 5}}, "max_tokens_per_hour"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.m.Enabled = true
			err := c.m.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, c.want)
			}
			c.m.Enabled = false
			if err := c.m.Validate(); err != nil {
				t.Fatalf("a disabled block must validate, got %v", err)
			}
		})
	}
	t.Run("ipv6 loopback", func(t *testing.T) {
		m := MeetingsConfig{Enabled: true, Quorum: MeetingsQuorumConfig{Listen: "[::1]:8765"}}
		if err := m.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
}

func TestMeetingsConfigYAMLKeys(t *testing.T) {
	src := `
enabled: true
quorum: {binary: /q.mjs, listen: "127.0.0.1:1"}
vexa: {url: "https://v", api_key_file: /data/secrets/k}
discord: {token_file: /data/secrets/d}
transcribe_url: https://w
transcribe_token_file: /data/secrets/t
delivery: chat
voice_on_address: false
bot_name: b
invite_policy: owner
research_pool_size: 2
research_per_meeting: 3
chat_posts_per_minute: 4
lobby_timeout_s: 5
budget: {max_tokens_per_meeting: 6, max_tokens_per_hour: 7, max_tokens_per_day: 8, max_intents_per_meeting: 9}
retention_days: 10
consent_text: c
data_dir: /d
`
	var m MeetingsConfig
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatal(err)
	}
	if !m.Enabled || m.Quorum.Listen != "127.0.0.1:1" || m.Vexa.APIKeyFile != "/data/secrets/k" || m.Discord.TokenFile != "/data/secrets/d" ||
		m.TranscribeTokenFile != "/data/secrets/t" ||
		m.VoiceOnAddress == nil || *m.VoiceOnAddress || m.Budget.MaxIntentsPerMeeting != 9 || m.DataDir != "/d" || m.LobbyTimeoutS != 5 {
		t.Fatalf("yaml keys not decoded: %+v", m)
	}
}

func TestMeetingsSecrets(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	key := filepath.Join(dir, "secrets", "vexa")
	tok := filepath.Join(dir, "secrets", "discord")
	tx := filepath.Join(dir, "secrets", "transcribe")
	m := MeetingsConfig{Vexa: MeetingsVexaConfig{APIKeyFile: key}, Discord: MeetingsDiscordConfig{TokenFile: tok}, TranscribeTokenFile: tx}

	if v, err := m.VexaAPIKey(ctx); err != nil || v != "" {
		t.Fatalf("missing file: %q, %v; want empty and no error", v, err)
	}
	if err := WriteMeetingsSecret(ctx, key, "  tenant-key \n"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode: %v, %v", fi, err)
	}
	if v, err := m.VexaAPIKey(ctx); err != nil || v != "tenant-key" {
		t.Fatalf("VexaAPIKey = %q, %v", v, err)
	}
	if err := WriteMeetingsSecret(ctx, tok, "bot-token"); err != nil {
		t.Fatal(err)
	}
	if v, err := m.DiscordToken(ctx); err != nil || v != "bot-token" {
		t.Fatalf("DiscordToken = %q, %v", v, err)
	}
	if v, err := m.TranscribeToken(ctx); err != nil || v != "" {
		t.Fatalf("missing transcribe token file: %q, %v; want empty and no error", v, err)
	}
	if err := WriteMeetingsSecret(ctx, tx, " transcribe-token\n"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(tx); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("transcribe token file mode: %v, %v", fi, err)
	}
	if v, err := m.TranscribeToken(ctx); err != nil || v != "transcribe-token" {
		t.Fatalf("TranscribeToken = %q, %v", v, err)
	}

	t.Setenv(MeetingsVexaAPIKeyEnv, "from-env")
	t.Setenv(MeetingsDiscordTokenEnv, "tok-env")
	t.Setenv(MeetingsTranscribeTokenEnv, "tx-env")
	if v, _ := m.TranscribeToken(ctx); v != "tx-env" {
		t.Fatalf("env transcribe token must win, got %q", v)
	}
	if v, _ := m.VexaAPIKey(ctx); v != "from-env" {
		t.Fatalf("env key must win, got %q", v)
	}
	if v, _ := m.DiscordToken(ctx); v != "tok-env" {
		t.Fatalf("env token must win, got %q", v)
	}
	t.Setenv(MeetingsVexaAPIKeyEnv, "")

	// Clearing removes the file; clearing twice is fine.
	if err := WriteMeetingsSecret(ctx, key, ""); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeetingsSecret(ctx, key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("cleared secret still present: %v", err)
	}

	// A directory where the file should be is a read error, not "unset".
	if err := os.MkdirAll(key, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.VexaAPIKey(ctx); err == nil {
		t.Fatal("want an error reading a directory as a secret")
	}
	// Removing a non-empty directory fails.
	if err := os.WriteFile(filepath.Join(key, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeetingsSecret(ctx, key, ""); err == nil {
		t.Fatal("want an error removing a non-empty directory")
	}
	// The parent cannot be created under a regular file.
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeetingsSecret(ctx, filepath.Join(blocker, "s"), "v"); err == nil {
		t.Fatal("want an error when the secret dir cannot be created")
	}
}

func TestResolveMeetingsLLM(t *testing.T) {
	ctx := context.Background()
	if r := (*Config)(nil).ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone || r.Label() != MeetingsLLMNoModelReason {
		t.Fatalf("nil config %+v", r)
	}
	dir := t.TempDir()
	cfg := &Config{}
	cfg.Meetings.LLM.APIKeyFile = filepath.Join(dir, "missing")
	cfg.Meetings.LLM.AnthropicKeyFile = filepath.Join(dir, "missing-a")
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone {
		t.Fatalf("nothing configured %+v", r)
	}
	// Falls back to governor.litellm like the knowledge advisor.
	cfg.Governor.LiteLLM.Endpoint = "https://litellm.example/"
	cfg.Governor.LiteLLM.DefaultModel = "gpt-x"
	r := cfg.ResolveMeetingsLLM(ctx)
	if r.Backend != MeetingsLLMModeOpenAI || r.Endpoint != "https://litellm.example" || r.Model != "gpt-x" ||
		r.TimeoutMS != DefaultMeetingsLLMTimeoutMS || r.MaxInFlight != DefaultMeetingsLLMMaxInFlight || r.Label() != "model gpt-x" ||
		r.PerMeetingInFlight != DefaultMeetingsLLMPerMeetingInFlight {
		t.Fatalf("litellm fallback %+v", r)
	}
	// Meetings' own endpoint, model and key win.
	keyFile := filepath.Join(dir, "k")
	if err := WriteMeetingsSecret(ctx, keyFile, "mkey"); err != nil {
		t.Fatal(err)
	}
	cfg.Meetings.LLM = MeetingsLLMConfig{Endpoint: "https://llm.example", Model: "m1", APIKeyFile: keyFile, TimeoutMS: 900, MaxInFlight: 2,
		AnthropicKeyFile: filepath.Join(dir, "missing-a")}
	r = cfg.ResolveMeetingsLLM(ctx)
	if r.Endpoint != "https://llm.example" || r.Model != "m1" || r.APIKey != "mkey" || r.TimeoutMS != 900 || r.MaxInFlight != 2 {
		t.Fatalf("meetings llm %+v", r)
	}
	// One meeting never runs more calls than the hive-wide cap.
	if r.PerMeetingInFlight != 2 {
		t.Fatalf("per-meeting in flight %d above max_in_flight 2", r.PerMeetingInFlight)
	}
	// An Anthropic key switches to anthropic mode.
	aFile := filepath.Join(dir, "a")
	if err := WriteMeetingsSecret(ctx, aFile, "akey"); err != nil {
		t.Fatal(err)
	}
	cfg.Meetings.LLM.AnthropicKeyFile = aFile
	r = cfg.ResolveMeetingsLLM(ctx)
	if r.Backend != MeetingsLLMModeAnthropic || r.AnthropicKey != "akey" || !strings.Contains(r.Label(), "Anthropic") {
		t.Fatalf("anthropic %+v", r)
	}
	// The persona answers with meetings.llm.model on the Anthropic path,
	// else the fast default.
	if r.Model != "m1" {
		t.Fatalf("anthropic persona model %q", r.Model)
	}
	saved := cfg.Meetings.LLM.Model
	cfg.Meetings.LLM.Model = ""
	if r := cfg.ResolveMeetingsLLM(ctx); r.Model != DefaultMeetingsPersonaAnthropicModel {
		t.Fatalf("default anthropic persona model %q", r.Model)
	}
	cfg.Meetings.LLM.Model = saved
	cfg.Meetings.LLM.AnthropicModel = "claude-x"
	if r := cfg.ResolveMeetingsLLM(ctx); r.Label() != "Anthropic claude-x" {
		t.Fatal(r.Label())
	}
	// mode openai ignores the Anthropic key; mode none turns the model off.
	cfg.Meetings.LLM.Mode = "openai"
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeOpenAI {
		t.Fatalf("openai mode %+v", r)
	}
	t.Setenv(MeetingsLLMModeEnv, "none")
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone || !strings.Contains(r.Reason, "turned off") {
		t.Fatalf("none %+v", r)
	}
	t.Setenv(MeetingsLLMModeEnv, "anthropic")
	cfg.Meetings.LLM.AnthropicKeyFile = filepath.Join(dir, "missing-a")
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone || !strings.Contains(r.Reason, "Anthropic") {
		t.Fatalf("anthropic without key %+v", r)
	}
}

func TestMeetingsLLMValidate(t *testing.T) {
	m := MeetingsConfig{Enabled: true, LLM: MeetingsLLMConfig{Mode: "gpt"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "meetings.llm.mode") {
		t.Fatalf("mode = %v", err)
	}
	m.LLM = MeetingsLLMConfig{Endpoint: "ftp://x"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "meetings.llm.endpoint") {
		t.Fatalf("endpoint = %v", err)
	}
	m.LLM = MeetingsLLMConfig{TimeoutMS: -1}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "timeout_ms") {
		t.Fatalf("timeout = %v", err)
	}
	m.LLM = MeetingsLLMConfig{Mode: "anthropic", Endpoint: "https://x"}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The listening tone is on by default, the yaml key turns it off, and the
// environment wins over the yaml.
func TestListeningToneEnabled(t *testing.T) {
	off := false
	if !(MeetingsConfig{}).ListeningToneEnabled() {
		t.Fatal("default on")
	}
	if (MeetingsConfig{ListeningTone: &off}).ListeningToneEnabled() {
		t.Fatal("meetings.listening_tone false turns it off")
	}
	t.Setenv(MeetingsListeningToneEnv, "false")
	if (MeetingsConfig{}).ListeningToneEnabled() {
		t.Fatal("env false turns it off")
	}
	t.Setenv(MeetingsListeningToneEnv, "true")
	if !(MeetingsConfig{ListeningTone: &off}).ListeningToneEnabled() {
		t.Fatal("env true wins over yaml")
	}
}
