package config

import (
	"strings"
	"testing"
)

func TestInboxUploadsDefaultsAndOverrides(t *testing.T) {
	var u InboxUploads
	if u.EffectiveMaxFileBytes() != DefaultInboxUploadsMaxFileBytes || u.EffectiveQuotaBytes() != DefaultInboxUploadsQuotaBytes ||
		u.EffectiveMaxFilesPerDrop() != DefaultInboxUploadsMaxFilesPerDrop || u.EffectiveMediaRetentionDays() != DefaultInboxUploadsMediaRetentionDays ||
		u.EffectiveDocumentRetentionDays() != 0 {
		t.Fatalf("defaults = %d %d %d %d %d", u.EffectiveMaxFileBytes(), u.EffectiveQuotaBytes(), u.EffectiveMaxFilesPerDrop(), u.EffectiveMediaRetentionDays(), u.EffectiveDocumentRetentionDays())
	}
	u = InboxUploads{MaxFileBytes: 1 << 20, QuotaBytes: 1 << 30, MaxFilesPerDrop: 5, MediaRetentionDays: 2, DocumentRetentionDays: 90}
	if u.EffectiveMaxFileBytes() != 1<<20 || u.EffectiveQuotaBytes() != 1<<30 || u.EffectiveMaxFilesPerDrop() != 5 || u.EffectiveMediaRetentionDays() != 2 || u.EffectiveDocumentRetentionDays() != 90 {
		t.Fatal("configured values not used")
	}
	t.Setenv(EnvInboxUploadsMaxFileBytes, "1024")
	t.Setenv(EnvInboxUploadsQuotaBytes, "4096")
	t.Setenv(EnvInboxUploadsMaxFilesPerDrop, "3")
	t.Setenv(EnvInboxUploadsMediaRetentionDays, "1")
	t.Setenv(EnvInboxUploadsDocumentRetentionDays, "7")
	if u.EffectiveMaxFileBytes() != 1024 || u.EffectiveQuotaBytes() != 4096 || u.EffectiveMaxFilesPerDrop() != 3 || u.EffectiveMediaRetentionDays() != 1 || u.EffectiveDocumentRetentionDays() != 7 {
		t.Fatal("env overrides not used")
	}
	t.Setenv(EnvInboxUploadsMaxFileBytes, "-5")
	if u.EffectiveMaxFileBytes() != 1<<20 {
		t.Fatal("a non-positive env value must fall through")
	}
}

func TestValidateInboxUploads(t *testing.T) {
	if err := ValidateInbox(InboxConfig{Uploads: InboxUploads{MaxFileBytes: 10}}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []InboxUploads{{MaxFileBytes: -1}, {QuotaBytes: -1}, {MaxFilesPerDrop: -1}, {MediaRetentionDays: -1}, {DocumentRetentionDays: -1}} {
		if err := ValidateInbox(InboxConfig{Uploads: u}); err == nil || !strings.Contains(err.Error(), "inbox.uploads") {
			t.Errorf("%+v: err = %v", u, err)
		}
	}
}
