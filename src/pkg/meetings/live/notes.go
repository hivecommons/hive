package live

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// DefaultVault is the knowledge vault (channel) meeting notes go to.
const DefaultVault = "meetings"

// FactImporter is the slice of *knowledge.KnowledgeAPI the notes sink uses.
type FactImporter interface {
	ImportFacts(ctx context.Context, layer knowledge.LayerType, content string, format string) (int, error)
}

// KnowledgeNotes writes meeting facts into a knowledge vault. It goes
// through KnowledgeAPI.ImportFacts rather than CreateFact because a local
// vault (channel) write keyed by title replaces the earlier fact, which is
// how one fact per meeting is updated in place; ImportFacts falls back to a
// remote wiki layer of the same name, the same path CreateFact takes.
// Facts are marked sensitive so repository publishing never copies meeting
// content out of the hive.
type KnowledgeNotes struct {
	API   FactImporter
	Vault string
	Now   func() time.Time
}

// Upsert implements NotesSink.
func (k KnowledgeNotes) Upsert(ctx context.Context, title, body string, tags []string) error {
	vault := k.Vault
	if vault == "" {
		vault = DefaultVault
	}
	now := time.Now
	if k.Now != nil {
		now = k.Now
	}
	data, err := json.Marshal([]knowledge.ExtractedFact{{
		Title: title, Body: body, Type: knowledge.FactReference, Confidence: 1,
		Tags: tags, SourcePR: "meeting", SourceDate: now(), Sensitive: true,
	}})
	if err != nil {
		return err
	}
	_, err = k.API.ImportFacts(ctx, knowledge.LayerType(vault), string(data), "json")
	return err
}
