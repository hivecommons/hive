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
// content out of the hive. The channel is created on first use when the
// hive has none of that name (and no remote layer took the write): meeting
// content is never written into some other existing channel instead, where
// it would mix with unrelated knowledge and its audience.
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
	if err == nil || !k.createChannel(vault) {
		return err
	}
	// The channel did not exist (a hive where nobody made a "meetings"
	// channel: `no writable channel named "meetings"`, and every research
	// result and notes update was lost). It exists now: write again.
	_, err = k.API.ImportFacts(ctx, knowledge.LayerType(vault), string(data), "json")
	return err
}

// ChannelCreator is the slice of *knowledge.KnowledgeAPI that lists and
// creates local knowledge channels (vaults).
type ChannelCreator interface {
	WritableVaults() []knowledge.VaultInfo
	CreateVault(name string) (knowledge.VaultInfo, error)
}

// createChannel creates the notes channel when the knowledge API can and
// no channel of that name exists yet. It reports whether it created one,
// so a write that failed for another reason is not retried.
func (k KnowledgeNotes) createChannel(vault string) bool {
	cc, ok := k.API.(ChannelCreator)
	if !ok {
		return false
	}
	for _, v := range cc.WritableVaults() {
		if v.Name == vault {
			return false
		}
	}
	_, err := cc.CreateVault(vault)
	return err == nil
}
