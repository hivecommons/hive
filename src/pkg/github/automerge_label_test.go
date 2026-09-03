package github

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The queue label lands in someone else's repository, so a hive has to be able
// to name it. These pin the fallback chain rather than the literal, so a hive
// that changes the default does not have to change the tests too.

func TestAutoMergeLabelDefaultsWhenUnset(t *testing.T) {
	c := NewClient("fake", "acme", nil, nil, "")
	if got := c.AutoMergeLabel(); got != config.DefaultAutoMergeLabel {
		t.Fatalf("unconfigured client label = %q, want %q", got, config.DefaultAutoMergeLabel)
	}
}

func TestAutoMergeLabelHonoursConfiguration(t *testing.T) {
	c := NewClient("fake", "acme", nil, nil, "")
	c.SetAutoMergeLabel("ship-it")
	if got := c.AutoMergeLabel(); got != "ship-it" {
		t.Fatalf("configured label = %q, want %q", got, "ship-it")
	}
}

// A partially-populated config must not be able to produce an unnamed label:
// AddLabelsToIssue with "" is a 422, which would surface as a queue action that
// approves the PR and then fails, leaving it approved but never queued.
func TestAutoMergeLabelIgnoresBlank(t *testing.T) {
	c := NewClient("fake", "acme", nil, nil, "")
	c.SetAutoMergeLabel("ship-it")
	for _, blank := range []string{"", "   ", "\t\n"} {
		c.SetAutoMergeLabel(blank)
		if got := c.AutoMergeLabel(); got != "ship-it" {
			t.Fatalf("SetAutoMergeLabel(%q) cleared the label to %q", blank, got)
		}
	}
}

func TestAutoMergeLabelTrimsWhitespace(t *testing.T) {
	c := NewClient("fake", "acme", nil, nil, "")
	c.SetAutoMergeLabel("  lgtm  ")
	if got := c.AutoMergeLabel(); got != "lgtm" {
		t.Fatalf("label = %q, want %q", got, "lgtm")
	}
}

// A hive that booted without GitHub credentials runs with a nil *Client for the
// life of the process, and the dashboard still reports the label to the client
// via /api/role. Both accessors must survive that.
func TestAutoMergeLabelNilReceiver(t *testing.T) {
	var c *Client
	if got := c.AutoMergeLabel(); got != config.DefaultAutoMergeLabel {
		t.Fatalf("nil client label = %q, want %q", got, config.DefaultAutoMergeLabel)
	}
	c.SetAutoMergeLabel("ship-it") // must not panic
}
