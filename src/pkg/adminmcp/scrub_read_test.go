package adminmcp

import (
	"reflect"
	"testing"
)

// A read masked every value under a key containing token, secret, password,
// credential or authorization. settings_read then hid the name of the
// environment variable that supplies a secret (which the tool promises to
// show), a numeric token rate, token timestamps, a whole merge-policy block,
// and showed "[masked:token]" for a secret that is not set at all.
func TestScrubShowsValuesThatCannotBeASecret(t *testing.T) {
	hold := map[string]any{"org/repo": map[string]any{"enabled": true, "source": "config"}}
	for key, value := range map[string]any{
		// the name of the environment variable, not its value
		"token_env":        "GITLAB_TOKEN",
		"gitlabTokenEnv":   "GITLAB_TOKEN",
		"webhookSecretEnv": "HIVE_WEBHOOK_SECRET",
		"password_env_var": "SMTP_PASSWORD",
		"apiTokenEnvName":  "X_API_TOKEN_2",
		// counts and rates
		"tokens_per_coin":    float64(1000),
		"tokensIn":           float64(5),
		"max_tokens_per_day": float64(100),
		"tokens_by_model":    map[string]any{"opus": float64(7)},
		// timestamps
		"token_expires_at":   "2026-12-01T00:00:00Z",
		"token_issued_at":    "2026-11-01T10:00:00.123456789-05:00",
		"token_last_used_at": "2026-11-06T12:13:51Z",
		// nothing to mask
		"auth_token":         "",
		"registration_token": nil,
		"credentialWarnings": []any{},
		"client_secret":      map[string]any{},
		// the self-authorization hold is a merge policy
		"repoSelfAuthorizationHold": hold,
		"self_authorization_hold":   map[string]any{"enabled": false},
	} {
		out := Scrub(map[string]any{key: value}).(map[string]any)
		if !reflect.DeepEqual(out[key], value) {
			t.Errorf("%s: read shows %#v, want %#v", key, out[key], value)
		}
	}
}

func TestScrubStillMasksWhatCouldBeASecret(t *testing.T) {
	for name, tc := range map[string]struct {
		key   string
		value any
	}{
		"token":                         {"token", "opaque-value"},
		"auth token":                    {"auth_token", "abc123"},
		"registration token":            {"registration_token", "r-1"},
		"password":                      {"password", "hunter2"},
		"client secret":                 {"client_secret", "s3"},
		"authorization header":          {"Authorization", "opaque"},
		"proxy authorization":           {"proxy_authorization", "opaque"},
		"authorization hold, no self":   {"authorizationHold", "opaque"},
		"hold key that names a token":   {"self_authorization_hold_token", "opaque"},
		"env key, lower-case value":     {"token_env", "a3f1c9d2e4b5"},
		"env key, value with a dash":    {"tokenEnv", "sk-live-abc"},
		"env key, value with a space":   {"token_env", "MY TOKEN"},
		"env key, leading digit":        {"token_env", "9ABC"},
		"env-shaped value, not env key": {"api_token", "GITLAB_TOKEN"},
		"env as the whole key":          {"secretenv", "GITLAB_TOKEN"},
		"credential count":              {"access_tokens_per_user", float64(5)},
		"refresh token map":             {"refresh_tokens_by_user", map[string]any{"a": "b"}},
		"tokens with secret":            {"tokens_secret", map[string]any{"a": "b"}},
		"numeric under token secret":    {"token_secret", float64(123456)},
		"number under password":         {"password", float64(12345678)},
		"list of tokens":                {"refresh_tokens", []any{"opaque"}},
		"warnings with text":            {"credentialWarnings", []any{"a warning"}},
		"credential block":              {"credentialInjection", map[string]any{"enabled": true}},
		"almost a timestamp":            {"token", "2026-13-45T00:00:00Z"},
		"date only":                     {"token", "2026-11-06"},
	} {
		out := Scrub(map[string]any{tc.key: tc.value}).(map[string]any)
		got, _ := out[tc.key].(string)
		if len(got) < len("[masked:") || got[:len("[masked:")] != "[masked:" {
			t.Errorf("%s: %s is shown as %#v, want it masked", name, tc.key, out[tc.key])
		}
	}
}

// A value kept because of its key is still scrubbed by shape.
func TestScrubStillScrubsKeptValuesByShape(t *testing.T) {
	out := Scrub(map[string]any{
		"repoSelfAuthorizationHold": map[string]any{"org/repo": map[string]any{"reason": "see Bearer abcdefghijklmnopqrstuvwxyz012345", "api_token": "opaque"}},
	}).(map[string]any)
	repo := out["repoSelfAuthorizationHold"].(map[string]any)["org/repo"].(map[string]any)
	if repo["api_token"] != "[masked:token]" {
		t.Errorf("a token inside a shown block is not masked: %#v", repo)
	}
	if reason, _ := repo["reason"].(string); reason == "see Bearer abcdefghijklmnopqrstuvwxyz012345" {
		t.Errorf("a bearer token inside a shown block is not scrubbed: %q", reason)
	}
}

// The pending-confirmation scrub decides what a write may store. It is not
// loosened: these stay refused there.
func TestPendingScrubIsAsStrictAsBefore(t *testing.T) {
	out := scrubPendingMap(map[string]any{
		"token_env":        "GITLAB_TOKEN",
		"tokens_per_coin":  1000,
		"token_expires_at": "2026-12-01T00:00:00Z",
		"api_key":          "",
		"otelHeaders":      map[string]any{},
	}).(map[string]any)
	for key, value := range out {
		if text, _ := value.(string); len(text) < len("[masked:") || text[:len("[masked:")] != "[masked:" {
			t.Errorf("pending scrub now keeps %s = %#v", key, value)
		}
	}
}

func TestKeyWords(t *testing.T) {
	for key, want := range map[string][]string{
		"webhookSecretEnv": {"webhook", "secret", "env"},
		"token_env":        {"token", "env"},
		"TOKENS_PER_COIN":  {"tokens", "per", "coin"},
		"tokens-in":        {"tokens", "in"},
		"tokensecret":      {"tokensecret"},
		"oauth2Token":      {"oauth2", "token"},
	} {
		if got := keyWords(key); !reflect.DeepEqual(got, want) {
			t.Errorf("keyWords(%q) = %v, want %v", key, got, want)
		}
	}
}
