package main

import (
	"context"
	"testing"
	"time"
)

func TestVisualHiveLeaseDenialStopsAnUnexpiredToken(t *testing.T) {
	now := time.Now().UTC()
	runtime := &visualHiveTokenLeaseRuntime{token: "test-token", expiresAt: now.Add(time.Hour), now: func() time.Time { return now }}
	if err := runtime.Apply(nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Token(context.Background()); err != nil {
		t.Fatal("ordinary no-renewal heartbeat discarded valid token")
	}
	if err := runtime.Apply(nil, "the hosted hive has no assigned Visual Hive App"); err == nil {
		t.Fatal("Hub assignment denial was ignored")
	}
	if _, err := runtime.Token(context.Background()); err == nil {
		t.Fatal("denied token remained usable until expiration")
	}
	if !runtime.expiresAt.IsZero() {
		t.Fatal("denied lease was still advertised as current")
	}
}

func TestVisualHiveGitHubAppBrokerRequiresExplicitPerInstanceOptIn(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: "false", want: false},
		{value: "1", want: false},
		{value: " true ", want: true},
		{value: "TRUE", want: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv(visualHiveGitHubAppEnabledEnv, test.value)
			if got := visualHiveGitHubAppBrokerEnabled(); got != test.want {
				t.Fatalf("enabled=%t, want %t for %q", got, test.want, test.value)
			}
		})
	}
}
