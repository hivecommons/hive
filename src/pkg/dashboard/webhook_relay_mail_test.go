package dashboard

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func relayHeader(key, delivery string, ts time.Time, body []byte, withDelivery bool) http.Header {
	h := http.Header{}
	sec := strconv.FormatInt(ts.Unix(), 10)
	h.Set(webhookRelayTimestampHeader, sec)
	h.Set(webhookRelaySignatureHeader, signWebhookRelayForTest(key, sec, delivery, body))
	if withDelivery {
		h.Set(webhookRelayDeliveryHeader, delivery)
	}
	return h
}

// TestVerifyRelayedRequest: a non-GitHub relay (hive-mx mail, #18395) is
// bound to X-Hive-Relay-Delivery with the webhook relay's skew and replay
// rules.
func TestVerifyRelayedRequest(t *testing.T) {
	const key = "relay-key-mail"
	s := &Server{}
	now := time.Now()
	body := []byte(`{"id":"d1","kind":"invite"}`)

	if r := s.verifyRelayedRequest(relayHeader(key, "d1", now, body, true), body, key, now); r != "" {
		t.Fatalf("valid relay rejected: %s", r)
	}
	if r := s.verifyRelayedRequest(relayHeader(key, "d1", now, body, true), body, key, now); r != github.WebhookReasonRelayReplayed {
		t.Fatalf("replay: %q", r)
	}
	cases := map[string]struct {
		h    http.Header
		body []byte
		key  string
		want string
	}{
		"no key":      {relayHeader(key, "d2", now, body, true), body, "", github.WebhookReasonRelaySignatureFailed},
		"unsigned":    {http.Header{webhookRelayDeliveryHeader: {"d3"}}, body, key, github.WebhookReasonRelaySignatureFailed},
		"no delivery": {relayHeader(key, "d4", now, body, false), body, key, github.WebhookReasonRelayDeliveryMissing},
		"tampered":    {relayHeader(key, "d5", now, body, true), []byte(`{"id":"d5","kind":"files"}`), key, github.WebhookReasonRelaySignatureFailed},
		"other delivery": {func() http.Header {
			h := relayHeader(key, "d6", now, body, true)
			h.Set(webhookRelayDeliveryHeader, "d7")
			return h
		}(), body, key, github.WebhookReasonRelaySignatureFailed},
		"stale":           {relayHeader(key, "d8", now.Add(-time.Hour), body, true), body, key, github.WebhookReasonRelayStale},
		"from the future": {relayHeader(key, "d9", now.Add(time.Hour), body, true), body, key, github.WebhookReasonRelayStale},
	}
	for name, c := range cases {
		if got := s.verifyRelayedRequest(c.h, c.body, c.key, now); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// TestRelayReplayNamespaces: a mail delivery id never collides with a GitHub
// delivery id of the same value.
func TestRelayReplayNamespaces(t *testing.T) {
	const key = "relay-key-ns"
	s := &Server{}
	now := time.Now()
	body := []byte(`{}`)
	gh := relayHeader(key, "same-id", now, body, false)
	gh.Set("X-GitHub-Delivery", "same-id")
	if r := s.verifyWebhookRelay(gh, body, key, now); r != "" {
		t.Fatalf("github relay: %q", r)
	}
	if r := s.verifyRelayedRequest(relayHeader(key, "same-id", now, body, true), body, key, now); r != "" {
		t.Fatalf("mail relay with the same id: %q", r)
	}
}
