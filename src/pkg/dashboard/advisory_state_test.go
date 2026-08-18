package dashboard

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func newAdvisoryTestServer() *Server {
	return NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A fresh server has never posted a digest: the last-posted time is zero and
// the error is empty, which the heartbeat builder reports as an empty string —
// UNKNOWN to the hub, never a false stale alarm.
func TestAdvisoryState_ZeroBeforeAnyPost(t *testing.T) {
	s := newAdvisoryTestServer()
	postedAt, findings, errMsg := s.AdvisoryState()
	if !postedAt.IsZero() {
		t.Fatalf("expected zero lastPostedAt before any post, got %v", postedAt)
	}
	if findings != 0 {
		t.Fatalf("expected 0 findings before any post, got %d", findings)
	}
	if errMsg != "" {
		t.Fatalf("expected empty error before any post, got %q", errMsg)
	}
}

// RecordAdvisoryPost stamps the current time and the finding count, and clears
// any prior error — a fresh success means the advisory path is healthy again.
func TestRecordAdvisoryPost_StampsTimeAndFindings(t *testing.T) {
	s := newAdvisoryTestServer()
	s.RecordAdvisoryError("boom") // prior failure that a success must clear

	before := time.Now()
	s.RecordAdvisoryPost(7)
	after := time.Now()

	postedAt, findings, errMsg := s.AdvisoryState()
	if postedAt.Before(before) || postedAt.After(after) {
		t.Fatalf("lastPostedAt %v not within [%v,%v]", postedAt, before, after)
	}
	if findings != 7 {
		t.Fatalf("expected 7 findings, got %d", findings)
	}
	if errMsg != "" {
		t.Fatalf("a successful post must clear the prior error, got %q", errMsg)
	}
}

// RecordAdvisoryError records the cause but must NOT advance the last-successful
// post time — the digest's age has to keep growing so the hub still sees it go
// stale, while the error names the specific cause.
func TestRecordAdvisoryError_KeepsLastPostTime(t *testing.T) {
	s := newAdvisoryTestServer()
	s.RecordAdvisoryPost(3)
	postedAt1, _, _ := s.AdvisoryState()

	s.RecordAdvisoryError("403 Resource not accessible by integration")

	postedAt2, findings, errMsg := s.AdvisoryState()
	if !postedAt2.Equal(postedAt1) {
		t.Fatalf("error must not move lastPostedAt: was %v, now %v", postedAt1, postedAt2)
	}
	if findings != 3 {
		t.Fatalf("error must not change last findings count, got %d", findings)
	}
	if errMsg != "403 Resource not accessible by integration" {
		t.Fatalf("expected recorded error, got %q", errMsg)
	}
}
