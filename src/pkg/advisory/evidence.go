package advisory

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/logscrub"
)

// evidenceHashMetadataKey is the bead metadata key holding the hash of the
// finding text (title, detail, file reference) the bead most recently
// recorded. It is the no-provenance counterpart of provenanceSHAMetadataKey:
// where a provenance SHA states where evidence was computed, this identifies
// WHAT was reported, so a byte-identical re-report can be recognised as a
// cached replay rather than fresh confirmation (#5236).
const evidenceHashMetadataKey = "evidence_hash"

// evidenceReplayCountMetadataKey counts the identical no-provenance re-reports
// PersistAsBeads has skipped since this bead's evidence last changed. The
// digest renders a non-zero count as an unverified caption: repetition that
// used to read as "reported 3×, still happening" is really the same cached
// text replayed, and saying so is what stops a disproved finding and the
// downstream issue refuting it from both presenting as live work (#5236).
const evidenceReplayCountMetadataKey = "evidence_replay_count"

// findingEvidenceHash identifies WHAT a finding reports: a hash over its
// scrubbed title, detail and file reference. Two reports hash equal only when
// they are textually identical — the deliberately narrow definition of "cached
// replay". Anything the producer changed, even a run number in the title,
// hashes differently and keeps the conservative pre-#5236 behavior of counting
// as confirmation; only verbatim replay is stopped from refreshing forever.
//
// Fields are scrubbed before hashing so the hash stays stable across the
// scrub-on-write round trip findings take through bead metadata.
func findingEvidenceHash(f Finding) string {
	h := sha256.New()
	for _, part := range []string{
		strings.TrimSpace(logscrub.ScrubString(f.Title)),
		strings.TrimSpace(logscrub.ScrubString(f.Detail)),
		strings.TrimSpace(logscrub.ScrubString(f.File)),
		strconv.Itoa(f.Line),
	} {
		h.Write([]byte(part))
		// Field separator, so ("ab","c") and ("a","bc") hash differently.
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// beadWithIdenticalEvidence returns the OPEN advisory bead this report would
// land on when that bead already records exactly this evidence hash, or nil.
//
// Title matching mirrors provenanceAlreadyRecorded (exact, or equal under
// beads.UpsertTitleKey): the stored hash belongs to whichever finding last
// refreshed the bead, and after Upsert folded a cosmetic title drift that
// finding's title is not the bead's own — an exact-title gate would wave every
// replay of the drifted report through. An empty stored hash never matches:
// beads written before this key existed must take the refresh path once (which
// stamps the hash) rather than be judged on evidence nobody recorded.
func beadWithIdenticalEvidence(existing []*beads.Bead, title, hash string) *beads.Bead {
	key := beads.UpsertTitleKey(title)
	for _, b := range existing {
		if b.Type != beads.TypeAdvisory {
			continue
		}
		// A resolved bead never gates: if the condition recurs after healing,
		// the re-report has to open a fresh bead (#2575).
		if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
			continue
		}
		if b.Title != title && beads.UpsertTitleKey(b.Title) != key {
			continue
		}
		if h := b.Meta(evidenceHashMetadataKey); h != "" && h == hash {
			return b
		}
	}
	return nil
}
