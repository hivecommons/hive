package hub

import (
	"testing"

	"github.com/hivecommons/hive/pkg/persona"
)

func TestHubPersonaStoredOnSaaSUserRecord(t *testing.T) {
	oldDir := saasUsersDir
	saasUsersDir = t.TempDir()
	t.Cleanup(func() { saasUsersDir = oldDir })

	if err := saveUserPersona("github:alice", persona.Record{
		Depth:         "technical",
		SummaryLength: "detailed",
		Notes:         "prefers links to receipts",
	}); err != nil {
		t.Fatalf("saveUserPersona: %v", err)
	}

	got, ok := loadUserPersona("github:alice")
	if !ok {
		t.Fatal("loadUserPersona did not find saved record")
	}
	if got.Depth != persona.DepthTechnical || got.SummaryLength != persona.SummaryDetailed || got.Notes != "prefers links to receipts" {
		t.Fatalf("persona = %#v", got)
	}
	u := loadSaaSUser("github:alice")
	if u == nil || len(u.Hives) != 0 || u.SaaSQuota != 0 {
		t.Fatalf("persona write changed autonomy/access fields: %#v", u)
	}
}
