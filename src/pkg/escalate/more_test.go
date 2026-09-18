package escalate

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSeverityParsingAndNames(t *testing.T) {
	for _, in := range []string{"info", " Decision ", "PAGE"} {
		if _, ok := ParseSeverity(in); !ok {
			t.Fatalf("ParseSeverity(%q) failed", in)
		}
	}
	if _, ok := ParseSeverity("low"); ok {
		t.Fatal("invalid severity parsed")
	}
	if NewEmailSink(EmailConfig{}).Name() != "email" {
		t.Fatal("email name")
	}
	if (&NtfySink{}).Name() != "ntfy" || (&PagerDutySink{}).Name() != "pagerduty" {
		t.Fatal("push names")
	}
}

func TestDispatcherFailureAuditAndRegisterNoops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var audits atomic.Int32
	d := NewDispatcher(ctx, nil, func(action, detail, sink string) { audits.Add(1) })
	d.Register(nil, SeverityInfo, 0)
	d.Register(&fakeSink{name: "bad", fail: 2}, SeverityInfo, 0)
	d.Dispatch(Event{Severity: SeverityInfo, Title: "x"})
	waitFor(t, func() bool { return audits.Load() > 0 })
	d.Stop()
	d.Register(&fakeSink{name: "late"}, SeverityInfo, 0)
	NewDispatcher(nil, nil, nil).Stop()
	var nilDispatcher *Dispatcher
	nilDispatcher.Stop()
}

func TestEmailDigestStartAndEmptyCases(t *testing.T) {
	NewEmailSink(EmailConfig{}).StartDigest(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	NewEmailSink(EmailConfig{DigestTo: []string{"ops@example.com"}, DigestAt: "bad", Now: func() time.Time { return time.Now().Add(24 * time.Hour) }}).StartDigest(ctx)
	cancel()
	s := NewEmailSink(EmailConfig{DigestTo: []string{"ops@example.com"}, Now: func() time.Time { return time.Now().Add(24 * time.Hour) }})
	if err := s.Deliver(context.Background(), Event{Severity: SeverityInfo, Title: "info"}); err != nil {
		t.Fatal(err)
	}
	s.cfg.DigestTo = nil
	if err := s.SendDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.send(context.Background(), nil, "s", "b"); err != nil {
		t.Fatal(err)
	}
	s.cfg.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	s.recordDigest(Event{Severity: SeverityInfo, Title: "tomorrow"})
	bad := NewEmailSink(EmailConfig{Host: "127.0.0.1", Port: 1, From: "a@example.com", To: []string{"b@example.com"}})
	if err := bad.Deliver(context.Background(), Event{Severity: SeverityPage, Title: "page"}); err == nil {
		t.Fatal("want smtp dial error")
	}
}

func TestPushClientAndRequestErrors(t *testing.T) {
	errClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("net") })}
	if err := (&NtfySink{URL: "http://example", Client: errClient}).Deliver(context.Background(), Event{Severity: SeverityInfo, Title: "t"}); err == nil {
		t.Fatal("want ntfy error")
	}
	if err := (&PagerDutySink{URL: "http://example", Client: errClient}).Deliver(context.Background(), Event{Severity: SeverityDecision, Title: "t"}); err == nil {
		t.Fatal("want pagerduty error")
	}
	if err := (&PagerDutySink{URL: "http://%zz", RoutingKey: "r"}).Deliver(context.Background(), Event{Severity: SeverityPage, Title: "t"}); err == nil {
		t.Fatal("want pagerduty bad url error")
	}
	if err := (&PushoverSink{URL: "http://%zz", AppToken: "a", UserKey: "u"}).Deliver(context.Background(), Event{Severity: SeverityDecision, Title: "t"}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("bad url err=%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
