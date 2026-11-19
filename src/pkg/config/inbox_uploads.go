package config

import "fmt"

// Inbox upload defaults (inbox.uploads). Files and links dropped onto the
// Inbox are stored on this hive's own data volume; these bound what one hive
// keeps.
const (
	// DefaultInboxUploadsMaxFileBytes (25 MiB) caps one text document.
	DefaultInboxUploadsMaxFileBytes int64 = 25 << 20
	// DefaultInboxUploadsQuotaBytes (2 GiB) caps everything stored under
	// connector-state/uploads on one hive.
	DefaultInboxUploadsQuotaBytes int64 = 2 << 30
	// DefaultInboxUploadsMaxFilesPerDrop caps the files one drop or one
	// request may carry.
	DefaultInboxUploadsMaxFilesPerDrop = 20
	// DefaultInboxUploadsMediaRetentionDays deletes the original binary of an
	// audio or video upload after this many days (the transcript and the
	// item stay), matching the recordings retention.
	DefaultInboxUploadsMediaRetentionDays = 5
)

// Environment overrides for the upload limits (env > hive.yaml > default; a
// non-positive or malformed value falls through).
const (
	EnvInboxUploadsMaxFileBytes          = "HIVE_INBOX_UPLOADS_MAX_FILE_BYTES"
	EnvInboxUploadsQuotaBytes            = "HIVE_INBOX_UPLOADS_QUOTA_BYTES"
	EnvInboxUploadsMaxFilesPerDrop       = "HIVE_INBOX_UPLOADS_MAX_FILES_PER_DROP"
	EnvInboxUploadsMediaRetentionDays    = "HIVE_INBOX_UPLOADS_MEDIA_RETENTION_DAYS"
	EnvInboxUploadsDocumentRetentionDays = "HIVE_INBOX_UPLOADS_DOCUMENT_RETENTION_DAYS"
)

// InboxUploads is `inbox.uploads`: files and links the owner drops onto the
// Inbox. Every limit has a default, so the block is optional.
type InboxUploads struct {
	// MaxFileBytes caps one text document (default 25 MiB).
	MaxFileBytes int64 `yaml:"max_file_bytes,omitempty" json:"max_file_bytes,omitempty"`
	// QuotaBytes caps all uploads stored on this hive (default 2 GiB).
	QuotaBytes int64 `yaml:"quota_bytes,omitempty" json:"quota_bytes,omitempty"`
	// MaxFilesPerDrop caps the files of one drop (default 20).
	MaxFilesPerDrop int `yaml:"max_files_per_drop,omitempty" json:"max_files_per_drop,omitempty"`
	// MediaRetentionDays deletes the original audio/video binary after this
	// many days (default 5); the extracted text and the item stay.
	MediaRetentionDays int `yaml:"media_retention_days,omitempty" json:"media_retention_days,omitempty"`
	// DocumentRetentionDays deletes the original of a text document after
	// this many days; the extracted text and the item stay. 0 (default)
	// keeps originals.
	DocumentRetentionDays int `yaml:"document_retention_days,omitempty" json:"document_retention_days,omitempty"`
}

// EffectiveMaxFileBytes returns the per-file cap for text documents.
func (u InboxUploads) EffectiveMaxFileBytes() int64 {
	return inboxLimit(EnvInboxUploadsMaxFileBytes, u.MaxFileBytes, DefaultInboxUploadsMaxFileBytes)
}

// EffectiveQuotaBytes returns the per-hive upload quota.
func (u InboxUploads) EffectiveQuotaBytes() int64 {
	return inboxLimit(EnvInboxUploadsQuotaBytes, u.QuotaBytes, DefaultInboxUploadsQuotaBytes)
}

// EffectiveMaxFilesPerDrop returns the files-per-drop cap.
func (u InboxUploads) EffectiveMaxFilesPerDrop() int {
	return int(inboxLimit(EnvInboxUploadsMaxFilesPerDrop, int64(u.MaxFilesPerDrop), DefaultInboxUploadsMaxFilesPerDrop))
}

// EffectiveMediaRetentionDays returns how long original audio/video binaries
// are kept.
func (u InboxUploads) EffectiveMediaRetentionDays() int {
	return int(inboxLimit(EnvInboxUploadsMediaRetentionDays, int64(u.MediaRetentionDays), DefaultInboxUploadsMediaRetentionDays))
}

// EffectiveDocumentRetentionDays returns how long original text documents
// are kept; 0 keeps them.
func (u InboxUploads) EffectiveDocumentRetentionDays() int {
	return int(inboxLimit(EnvInboxUploadsDocumentRetentionDays, int64(u.DocumentRetentionDays), 0))
}

// ValidateInboxUploads rejects negative limits.
func ValidateInboxUploads(u InboxUploads) error {
	if u.MaxFileBytes < 0 || u.QuotaBytes < 0 || u.MaxFilesPerDrop < 0 || u.MediaRetentionDays < 0 || u.DocumentRetentionDays < 0 {
		return fmt.Errorf("inbox.uploads limits must not be negative")
	}
	return nil
}
