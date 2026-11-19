package config

import (
	"fmt"
	"time"
)

// Inbox recording defaults (`inbox.recording`): recording a meeting or a
// note straight from the dashboard into the Inbox. Every value is a named
// default the owner can override in hive.yaml.
const (
	// DefaultInboxRecordingEnabled shows the ● Record control. Recording is
	// still limited to owner and read-write sessions and needs the consent
	// notice acknowledged before every recording.
	DefaultInboxRecordingEnabled = true
	// DefaultInboxRecordingRetentionDays is how long the raw audio is kept
	// after a recording ends; the transcript and the Knowledge fact stay.
	DefaultInboxRecordingRetentionDays = 5
	// DefaultInboxRecordingMaxSeconds caps one recording (3 hours).
	DefaultInboxRecordingMaxSeconds = 3 * 60 * 60
	// DefaultInboxRecordingMaxBytes caps one recording's audio (200 MiB).
	DefaultInboxRecordingMaxBytes int64 = 200 << 20
	// DefaultInboxRecordingQuotaBytes caps all recording audio kept on this
	// hive at once (2 GiB).
	DefaultInboxRecordingQuotaBytes int64 = 2 << 30
	// DefaultInboxRecordingIdleFinalizeMinutes finalizes a recording whose
	// browser stopped sending chunks this long ago (2 hours).
	DefaultInboxRecordingIdleFinalizeMinutes = 120
)

// InboxRecording is `inbox.recording`.
type InboxRecording struct {
	// Enabled shows ● Record in the Inbox; nil uses
	// DefaultInboxRecordingEnabled.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// RetentionDays keeps the raw audio this many days after the recording
	// ends, then deletes it (0 uses the default).
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty"`
	// MaxSeconds caps one recording's length (0 uses the default).
	MaxSeconds int `yaml:"max_seconds,omitempty" json:"max_seconds,omitempty"`
	// MaxBytes caps one recording's audio size (0 uses the default).
	MaxBytes int64 `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
	// QuotaBytes caps the audio of all recordings kept at once (0 uses the
	// default).
	QuotaBytes int64 `yaml:"quota_bytes,omitempty" json:"quota_bytes,omitempty"`
	// IdleFinalizeMinutes finalizes a recording that received no chunk for
	// this long (0 uses the default).
	IdleFinalizeMinutes int `yaml:"idle_finalize_minutes,omitempty" json:"idle_finalize_minutes,omitempty"`
}

// EffectiveEnabled reports whether ● Record is offered.
func (r InboxRecording) EffectiveEnabled() bool {
	if r.Enabled == nil {
		return DefaultInboxRecordingEnabled
	}
	return *r.Enabled
}

// EffectiveRetentionDays returns the audio retention in days.
func (r InboxRecording) EffectiveRetentionDays() int {
	if r.RetentionDays > 0 {
		return r.RetentionDays
	}
	return DefaultInboxRecordingRetentionDays
}

// EffectiveRetention returns the audio retention as a duration.
func (r InboxRecording) EffectiveRetention() time.Duration {
	return time.Duration(r.EffectiveRetentionDays()) * 24 * time.Hour
}

// EffectiveMaxSeconds returns the longest recording allowed, in seconds.
func (r InboxRecording) EffectiveMaxSeconds() int {
	if r.MaxSeconds > 0 {
		return r.MaxSeconds
	}
	return DefaultInboxRecordingMaxSeconds
}

// EffectiveMaxBytes returns the per-recording audio cap.
func (r InboxRecording) EffectiveMaxBytes() int64 {
	if r.MaxBytes > 0 {
		return r.MaxBytes
	}
	return DefaultInboxRecordingMaxBytes
}

// EffectiveQuotaBytes returns the hive-wide recording audio cap.
func (r InboxRecording) EffectiveQuotaBytes() int64 {
	if r.QuotaBytes > 0 {
		return r.QuotaBytes
	}
	return DefaultInboxRecordingQuotaBytes
}

// EffectiveIdleFinalize returns how long a silent recording stays open.
func (r InboxRecording) EffectiveIdleFinalize() time.Duration {
	m := r.IdleFinalizeMinutes
	if m <= 0 {
		m = DefaultInboxRecordingIdleFinalizeMinutes
	}
	return time.Duration(m) * time.Minute
}

// ValidateInboxRecording rejects negative limits and a per-recording cap
// larger than the hive-wide quota.
func ValidateInboxRecording(r InboxRecording) error {
	if r.RetentionDays < 0 || r.MaxSeconds < 0 || r.MaxBytes < 0 || r.QuotaBytes < 0 || r.IdleFinalizeMinutes < 0 {
		return fmt.Errorf("inbox.recording limits must not be negative")
	}
	if r.EffectiveMaxBytes() > r.EffectiveQuotaBytes() {
		return fmt.Errorf("inbox.recording.max_bytes (%d) must not exceed inbox.recording.quota_bytes (%d)", r.EffectiveMaxBytes(), r.EffectiveQuotaBytes())
	}
	return nil
}
