package proxy

import (
	"net/http"
	"testing"
)

// Use ordinary HTTP messages with inert metadata. Each relay must pass the
// end-to-end field while removing the other fields before the next hop.
func relayTestRequestHeaders() http.Header {
	h := make(http.Header)
	setRelayTestResponseHeaders(h)
	h.Set("Proxy-Authorization", "Basic dGVzdDpmaXh0dXJl")
	h.Set("Proxy-Connection", "keep-alive")
	h.Set("x-forwarded-example", "fixture")
	h.Set("x-hive-example", "fixture")
	return h
}

func setRelayTestResponseHeaders(h http.Header) {
	h.Add("Connection", "Keep-Alive, x-relay-hop")
	h.Add("Connection", "X-Second-Hop")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("X-Relay-Hop", "fixture")
	h.Set("X-Second-Hop", "fixture")
	h.Set("X-End-To-End", "preserved")
}

func assertRelayHeaderHygiene(t *testing.T, got, sent http.Header) {
	t.Helper()
	for name := range sent {
		if name == "X-End-To-End" {
			if value := got.Get(name); value != "preserved" {
				t.Errorf("%s = %q, want preserved", name, value)
			}
		} else if len(got.Values(name)) != 0 {
			t.Errorf("%s was forwarded", name)
		}
	}
}

func assertRelayResponseHeaderHygiene(t *testing.T, resp *http.Response) {
	t.Helper()
	sent := make(http.Header)
	setRelayTestResponseHeaders(sent)
	assertRelayHeaderHygiene(t, resp.Header, sent)
}
