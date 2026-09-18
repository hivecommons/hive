package escalate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
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
	other := NewDispatcher(nil, nil, nil)
	if other.Context() == nil {
		t.Fatal("dispatcher context nil")
	}
	other.Stop()
	var nilDispatcher *Dispatcher
	nilDispatcher.Stop()
	if nilDispatcher.Context() == nil {
		t.Fatal("nil dispatcher context nil")
	}
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
	if bad.tlsConfig().ServerName != "127.0.0.1" {
		t.Fatal("default TLS server name not set")
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

func TestEmailDigestSpansMidnightAndCapsOldest(t *testing.T) {
	current := time.Date(2026, 9, 18, 23, 59, 0, 0, time.Local)
	s := NewEmailSink(EmailConfig{DigestTo: []string{"ops@example.com"}, Now: func() time.Time { return current }})
	s.recordDigest(Event{Severity: SeverityInfo, Title: "before midnight"})
	current = current.Add(2 * time.Minute)
	s.recordDigest(Event{Severity: SeverityInfo, Title: "after midnight"})
	if len(s.dig) != 2 {
		t.Fatalf("midnight rollover dropped events: %d", len(s.dig))
	}
	for i := 0; i < maxDigestEvents+3; i++ {
		s.recordDigest(Event{Severity: SeverityInfo, Title: fmt.Sprintf("event-%d", i)})
	}
	if len(s.dig) != maxDigestEvents {
		t.Fatalf("digest cap len=%d", len(s.dig))
	}
	if s.dropped == 0 {
		t.Fatal("expected dropped counter")
	}
}

func TestEmailRequiresSTARTTLSOnSubmissionPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	messages := make(chan string, 1)
	go func() { _ = ServeSMTPFake(context.Background(), ln, messages) }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := EmailConfig{Host: "127.0.0.1", From: "hive@example.com", To: []string{"ops@example.com"}}
	_, _ = fmt.Sscanf(port, "%d", &cfg.Port)
	err = NewEmailSink(cfg).Deliver(context.Background(), Event{Severity: SeverityPage, Title: "page"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("error=%v, want STARTTLS refusal", err)
	}
}

func TestEmailConcurrentDigestAndDeliverRace(t *testing.T) {
	s := NewEmailSink(EmailConfig{DigestTo: []string{"ops@example.com"}})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.Deliver(context.Background(), Event{Severity: SeverityInfo, Title: "info"})
		}()
		go func() { defer wg.Done(); _ = s.SendDigest(context.Background()) }()
	}
	wg.Wait()
}
