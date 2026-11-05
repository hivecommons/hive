package notify

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func expireNtfyCooldown(n *Notifier) {
	n.mu.Lock()
	n.ntfyRetryAt = time.Now().Add(-time.Second)
	n.mu.Unlock()
}

func TestSendNtfyRateLimitCooldown(t *testing.T) {
	future := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	for _, tc := range []struct {
		name       string
		retryAfter string
		wantDelay  time.Duration
		wantDate   time.Time
	}{
		{name: "seconds", retryAfter: "120", wantDelay: 2 * time.Minute},
		{name: "date", retryAfter: future.Format(http.TimeFormat), wantDate: future},
		{name: "missing", wantDelay: time.Minute},
		{name: "invalid", retryAfter: "later", wantDelay: time.Minute},
		{name: "zero", retryAfter: "0", wantDelay: time.Minute},
		{name: "negative", retryAfter: "-1", wantDelay: time.Minute},
		{name: "overflow", retryAfter: "9223372036854775807", wantDelay: time.Minute},
		{name: "past date", retryAfter: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), wantDelay: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					w.Header().Set("Retry-After", tc.retryAfter)
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			n := New(config.NotificationsConfig{
				Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "test"},
			}, silentLogger())

			before := time.Now()
			n.sendNtfy("title", "message", PriorityDefault)
			after := time.Now()
			n.mu.Lock()
			retryAt := n.ntfyRetryAt
			n.mu.Unlock()
			if !tc.wantDate.IsZero() {
				if !retryAt.Equal(tc.wantDate) {
					t.Fatalf("retry deadline = %v, want %v", retryAt, tc.wantDate)
				}
			} else if retryAt.Before(before.Add(tc.wantDelay)) || retryAt.After(after.Add(tc.wantDelay)) {
				t.Fatalf("retry deadline = %v, want delay %v after response", retryAt, tc.wantDelay)
			}

			// Concurrent callers must all observe the established cooldown.
			var wg sync.WaitGroup
			for range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					n.sendNtfy("suppressed", "message", PriorityHigh)
				}()
			}
			wg.Wait()
			if got := requests.Load(); got != 1 {
				t.Fatalf("requests during cooldown = %d, want 1", got)
			}

			expireNtfyCooldown(n)
			n.sendNtfy("resumed", "message", PriorityDefault)
			n.sendNtfy("next", "message", PriorityDefault)
			if got := requests.Load(); got != 3 {
				t.Fatalf("requests after cooldown = %d, want 3", got)
			}
		})
	}
}

func TestSendNtfyRateLimitFallbackBackoffAndReset(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	n := New(config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "test"},
	}, silentLogger())

	for _, want := range []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour,
	} {
		expireNtfyCooldown(n)
		before := time.Now()
		n.sendNtfy("title", "message", PriorityDefault)
		after := time.Now()
		n.mu.Lock()
		retryAt := n.ntfyRetryAt
		n.mu.Unlock()
		if retryAt.Before(before.Add(want)) || retryAt.After(after.Add(want)) {
			t.Fatalf("retry deadline = %v, want backoff %v after response", retryAt, want)
		}
	}

	expireNtfyCooldown(n)
	status.Store(http.StatusOK)
	n.sendNtfy("success", "message", PriorityDefault)
	status.Store(http.StatusTooManyRequests)
	before := time.Now()
	n.sendNtfy("rate limited again", "message", PriorityDefault)
	after := time.Now()
	n.mu.Lock()
	retryAt := n.ntfyRetryAt
	n.mu.Unlock()
	if retryAt.Before(before.Add(time.Minute)) || retryAt.After(after.Add(time.Minute)) {
		t.Fatalf("retry deadline after success = %v, want one-minute fallback", retryAt)
	}
}

func TestSendNtfyOtherErrorsDoNotStartCooldown(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(status)
		}))
		n := New(config.NotificationsConfig{
			Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "test"},
		}, silentLogger())
		n.sendNtfy("first", "message", PriorityDefault)
		n.sendNtfy("second", "message", PriorityDefault)
		srv.Close()
		if got := requests.Load(); got != 2 {
			t.Errorf("status %d: requests = %d, want 2", status, got)
		}
	}
}

func TestSendNtfyInFlightResponsePreservesCooldown(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusOK} {
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Title") == "slow" {
				close(started)
				<-release
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(status)
				return
			}
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()
		defer unblock()
		n := New(config.NotificationsConfig{
			Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "test"},
		}, silentLogger())
		done := make(chan struct{})
		go func() {
			n.sendNtfy("slow", "message", PriorityDefault)
			close(done)
		}()
		select {
		case <-started:
		case <-time.After(asyncWaitTimeout):
			t.Fatal("in-flight request did not reach ntfy")
		}
		n.sendNtfy("fast", "message", PriorityDefault)
		n.mu.Lock()
		wantDeadline := n.ntfyRetryAt
		n.mu.Unlock()
		unblock()
		select {
		case <-done:
		case <-time.After(asyncWaitTimeout):
			t.Fatal("in-flight request did not complete")
		}
		n.mu.Lock()
		gotDeadline := n.ntfyRetryAt
		n.mu.Unlock()
		if !gotDeadline.Equal(wantDeadline) {
			t.Errorf("status %d: in-flight response changed deadline from %v to %v", status, wantDeadline, gotDeadline)
		}
	}
}

func TestSendNtfyCooldownPreservesOtherChannels(t *testing.T) {
	ntfySrv := errorServer(http.StatusTooManyRequests)
	defer ntfySrv.Close()
	slackSrv, slackReqs, slackMu := mockServer(t)
	defer slackSrv.Close()
	discordSrv, discordReqs, discordMu := mockServer(t)
	defer discordSrv.Close()
	n := New(config.NotificationsConfig{
		Ntfy:    &config.NtfyConfig{Server: ntfySrv.URL, Topic: "test"},
		Slack:   &config.SlackConfig{Webhook: slackSrv.URL},
		Discord: &config.DiscordConfig{Webhook: discordSrv.URL},
	}, silentLogger())
	n.sendNtfy("establish cooldown", "message", PriorityDefault)
	n.Send("during cooldown", "message", PriorityHigh)
	if !waitForN(1, slackReqs, slackMu, asyncWaitTimeout) {
		t.Error("Slack delivery blocked during ntfy cooldown")
	}
	if !waitForN(1, discordReqs, discordMu, asyncWaitTimeout) {
		t.Error("Discord delivery blocked during ntfy cooldown")
	}
}
