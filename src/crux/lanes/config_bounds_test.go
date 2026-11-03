package lanes

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

// Every bounds check must name its own field and allowed range, so an operator
// reading the error knows which of the ~17 limits tripped. Each case drives one
// check out of range and asserts the message carries that field's name and
// range text, not a neighbour's.
func TestValidateBoundsNamesEveryFieldAndRange(t *testing.T) {
	tests := []struct {
		field  string
		rng    string
		change func(*Config)
	}{
		{"namespace", fmt.Sprintf("a lowercase identifier of at most %d characters", MaxIDLength), func(c *Config) { c.Namespace = "Not-Valid" }},
		{"retention", fmt.Sprintf("between %s and %s", MinRetention, MaxRetention), func(c *Config) { c.Retention = MaxRetention + time.Second }},
		{"dedup", fmt.Sprintf("between %s and retention", MinDedup), func(c *Config) { c.Dedup = c.Retention + time.Second }},
		{"lease", fmt.Sprintf("between %s and %s", MinLease, MaxLease), func(c *Config) { c.Lease = MaxLease + time.Second }},
		{"ack_retention", "between lease", func(c *Config) { c.AckRetention = c.Lease - time.Millisecond }},
		{"timeout", fmt.Sprintf("greater than 0 and at most %s", MaxTimeout), func(c *Config) { c.Timeout = MaxTimeout + time.Second }},
		{"retry_delay", fmt.Sprintf("at least %s", MinRetryDelay), func(c *Config) { c.RetryDelay = MinRetryDelay - time.Millisecond }},
		{"max_retry_delay", "between retry_delay", func(c *Config) { c.MaxRetryDelay = c.RetryDelay - time.Millisecond }},
		{"max_payload", fmt.Sprintf("between 1 and %d", MaxPayload), func(c *Config) { c.MaxPayload = MaxPayload + 1 }},
		{"max_messages", fmt.Sprintf("between 1 and %d", MaxLaneMessages), func(c *Config) { c.MaxMessages = 0 }},
		{"max_bytes", "between max_payload", func(c *Config) { c.MaxBytes = int64(c.MaxPayload) - 1 }},
		{"max_kv_bytes", fmt.Sprintf("between 1 and %d", MaxStorageBytes), func(c *Config) { c.MaxKVBytes = MaxStorageBytes + 1 }},
		{"max_pending", "at least 1", func(c *Config) { c.MaxPending = 0 }},
		{"max_ledger", "between max_pending", func(c *Config) { c.MaxLedger = c.MaxPending - 1 }},
		{"replicas", fmt.Sprintf("between 1 and %d", MaxReplicas), func(c *Config) { c.Replicas = MaxReplicas + 1 }},
		{"lanes", fmt.Sprintf("at most %d entries", DefaultMaxLanes), func(c *Config) {
			c.Lanes = make([]api.Lane, DefaultMaxLanes+1)
		}},
		{"principals", fmt.Sprintf("between 1 and %d entries", DefaultMaxMembers), func(c *Config) { c.Principals = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			c := Config{URL: "nats://localhost:4222", Principals: []Principal{{Project: "atlas", TokenEnv: "ATLAS_TOKEN"}}}
			c.Defaults()
			if err := c.Validate(); err != nil {
				t.Fatal("baseline configuration must validate before the field is driven out of range:", err)
			}
			tt.change(&c)
			err := c.Validate()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("out-of-range %s accepted: %v", tt.field, err)
			}
			want := "lanes: " + tt.field + " must be " + tt.rng
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error does not name %s with its range\n want substring: %q\n  got: %v", tt.field, want, err)
			}
		})
	}
}

// bound must not pay for formatting the range or value unless the check fails.
func TestBoundFormatsOnlyWhenViolated(t *testing.T) {
	calls := 0
	text := func() (string, any) { calls++; return "positive", -1 }
	if err := bound("x", false, text); err != nil || calls != 0 {
		t.Fatalf("satisfied bound formatted or errored: err=%v calls=%d", err, calls)
	}
	err := bound("x", true, text)
	if !errors.Is(err, ErrInvalid) || calls != 1 || err.Error() != "invalid lane request: lanes: x must be positive, got -1" {
		t.Fatalf("violated bound: err=%v calls=%d", err, calls)
	}
}
