package config

import (
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func TestInferenceResilienceResolve_DefaultsAreOff(t *testing.T) {
	for _, c := range []*InferenceResilienceConfig{nil, {}} {
		got, ignored := c.Resolve(noEnv)
		if got.Enabled() || got.TimeoutsEnabled() || got.RetryEnabled || len(ignored) != 0 {
			t.Fatalf("default must be off, got %+v ignored %v", got, ignored)
		}
		if got.MaxRetries != DefaultInferenceRetryMaxRetries ||
			got.BaseDelay != DefaultInferenceRetryBaseDelayMS*time.Millisecond ||
			got.MaxDelay != DefaultInferenceRetryMaxDelayS*time.Second {
			t.Fatalf("retry defaults not applied: %+v", got)
		}
	}
	if DefaultInferenceConnectTimeoutS != 0 || DefaultInferenceResponseHeaderTimeoutS != 0 ||
		DefaultInferenceIdleStreamTimeoutS != 0 || DefaultInferenceRetryEnabled {
		t.Fatal("every default must stay off")
	}
}

func TestInferenceResilienceResolve_YAMLEnvAndClamps(t *testing.T) {
	c := &InferenceResilienceConfig{
		ConnectTimeoutS: 3, ResponseHeaderTimeoutS: 30, IdleStreamTimeoutS: 60,
		RetryEnabled: true, RetryMaxRetries: 99, RetryBaseDelayMS: 250, RetryMaxDelayS: 10,
	}
	got, _ := c.Resolve(noEnv)
	if got.ConnectTimeout != 3*time.Second || got.ResponseHeaderTimeout != 30*time.Second ||
		got.IdleStreamTimeout != 60*time.Second || !got.RetryEnabled ||
		got.MaxRetries != MaxInferenceRetryMaxRetries ||
		got.BaseDelay != 250*time.Millisecond || got.MaxDelay != 10*time.Second {
		t.Fatalf("yaml not resolved or not clamped: %+v", got)
	}

	env := map[string]string{
		EnvInferenceConnectTimeoutS:        "5",
		EnvInferenceResponseHeaderTimeoutS: "bad",
		EnvInferenceIdleStreamTimeoutS:     "-1",
		EnvInferenceRetryEnabled:           "false",
		EnvInferenceRetryMaxRetries:        "1",
		EnvInferenceRetryBaseDelayMS:       "100",
		EnvInferenceRetryMaxDelayS:         "999999",
	}
	got, ignored := c.Resolve(func(k string) string { return env[k] })
	if got.ConnectTimeout != 5*time.Second || got.ResponseHeaderTimeout != 30*time.Second ||
		got.IdleStreamTimeout != 60*time.Second || got.RetryEnabled || got.MaxRetries != 1 ||
		got.BaseDelay != 100*time.Millisecond || got.MaxDelay != MaxInferenceTimeoutS*time.Second {
		t.Fatalf("env override wrong: %+v", got)
	}
	if len(ignored) != 2 {
		t.Fatalf("want the two invalid vars reported, got %v", ignored)
	}

	onlyEnv, _ := (*InferenceResilienceConfig)(nil).Resolve(func(k string) string {
		if k == EnvInferenceRetryEnabled {
			return "true"
		}
		return ""
	})
	if !onlyEnv.RetryEnabled || !onlyEnv.Enabled() {
		t.Fatal("env alone must be able to enable retry")
	}
	if onlyEnv.Summary() == "" {
		t.Fatal("summary must not be empty")
	}
}
