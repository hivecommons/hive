package config

import (
	"strings"
	"testing"
	"time"
)

func TestReviewEventDrivenEnabled(t *testing.T) {
	on, off := true, false
	tests := []struct {
		name     string
		cfg      ReviewConfig
		webhooks bool
		want     bool
	}{
		{"unset follows webhooks on", ReviewConfig{RequireApproval: true, FanOut: true}, true, true},
		{"unset follows webhooks off", ReviewConfig{RequireApproval: true, FanOut: true}, false, false},
		{"explicit off wins", ReviewConfig{RequireApproval: true, FanOut: true, EventDriven: &off}, true, false},
		{"explicit on without webhooks", ReviewConfig{RequireApproval: true, FanOut: true, EventDriven: &on}, false, true},
		{"swarm off: no approval gate", ReviewConfig{FanOut: true, EventDriven: &on}, true, false},
		{"swarm off: no fan-out", ReviewConfig{RequireApproval: true, EventDriven: &on}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ReviewEventDrivenEnabled(tc.webhooks); got != tc.want {
				t.Fatalf("ReviewEventDrivenEnabled(%v) = %v, want %v", tc.webhooks, got, tc.want)
			}
		})
	}
}

func TestReviewEventDebounce(t *testing.T) {
	tests := []struct {
		s    int
		want time.Duration
	}{
		{0, DefaultReviewEventDebounceS * time.Second},
		{-5, DefaultReviewEventDebounceS * time.Second},
		{30, 30 * time.Second},
	}
	for _, tc := range tests {
		if got := (ReviewConfig{EventDebounceS: tc.s}).ReviewEventDebounce(); got != tc.want {
			t.Fatalf("ReviewEventDebounce(%d) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestValidateReviewEventDispatch(t *testing.T) {
	tests := []struct {
		s       int
		wantErr bool
	}{
		{0, false},
		{90, false},
		{MaxReviewEventDebounceS, false},
		{-1, true},
		{MaxReviewEventDebounceS + 1, true},
	}
	for _, tc := range tests {
		err := (ReviewConfig{EventDebounceS: tc.s}).ValidateReviewEventDispatch()
		if (err != nil) != tc.wantErr {
			t.Fatalf("ValidateReviewEventDispatch(%d) err = %v, wantErr %v", tc.s, err, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "event_debounce_s") {
			t.Fatalf("error does not name the key: %v", err)
		}
	}
}
