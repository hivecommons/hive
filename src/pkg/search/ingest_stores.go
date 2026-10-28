package search

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// externalRefRE matches bead external refs of the form owner/repo#N.
var externalRefRE = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+)$`)

// ingestBeads indexes every bead of every store, keyed on the bead's
// updated_at (its revision): an unchanged bead is not re-indexed. Beads are
// copied out under the store's read lock (ReadEach) and never written.
func (i *Indexer) ingestBeads(r *rebuild) {
	if i.Beads == nil {
		return
	}
	stores := i.Beads()
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		store := stores[name]
		if store == nil {
			continue
		}
		var copied []beads.Bead
		store.ReadEach(beads.ListFilter{}, func(b *beads.Bead) {
			c := *b
			c.Metadata = nil
			c.DependsOn = nil
			copied = append(copied, c)
		})
		for _, b := range copied {
			i.ingestBead(r, name, b)
		}
	}
}

func (i *Indexer) ingestBead(r *rebuild, store string, b beads.Bead) {
	key := "bead:" + store + "/" + b.ID
	at := firstNonZero(b.UpdatedAt.Time, b.CreatedAt.Time)
	stamp := timeStamp(at) + "|" + string(b.Status)
	if i.unchanged(r, key, stamp) {
		return
	}
	repo, url := i.HomeRepo, ""
	if m := externalRefRE.FindStringSubmatch(strings.TrimSpace(b.ExternalRef)); m != nil {
		repo, url = m[1], fmt.Sprintf("https://github.com/%s/issues/%s", m[1], m[2])
	}
	if repo == "" || at.IsZero() {
		i.put(r, key, &sourceEntry{stamp: stamp}, nil)
		return
	}
	if url == "" {
		url = "bead://" + store + "/" + b.ID
	}
	doc := Document{
		ID: "bead/" + store + "/" + b.ID, Repo: repo, Kind: KindBead, At: at, URL: url,
		Title: b.Title, Text: b.Notes,
		Fields: map[string]string{"status": string(b.Status), "type": string(b.Type), "agent": store,
			"priority": strconv.Itoa(int(b.Priority)), "external_ref": b.ExternalRef},
	}
	i.put(r, key, &sourceEntry{stamp: stamp}, []Document{doc})
}

// ingestFacts indexes knowledge facts under HomeRepo, keyed on a hash of the
// indexed content. A fact without an Updated time is dated when the indexer
// first saw it, so retention still applies.
func (i *Indexer) ingestFacts(facts []knowledge.Fact, r *rebuild) {
	if i.HomeRepo == "" {
		return
	}
	for _, f := range facts {
		if f.Slug == "" {
			continue
		}
		key := "fact:" + string(f.Layer) + "/" + f.Slug
		h := sha256.New()
		for _, part := range []string{f.Title, f.Body, string(f.Type), f.Status, string(f.State), strings.Join(f.Tags, ","),
			timeStamp(f.Updated)} {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
		stamp := hex.EncodeToString(h.Sum(nil))
		if i.unchanged(r, key, stamp) {
			continue
		}
		at := f.Updated
		if at.IsZero() {
			at = r.now
			if prev := i.entries[key]; prev != nil && !prev.first.IsZero() {
				at = prev.first
			}
		}
		doc := Document{
			ID: "fact/" + string(f.Layer) + "/" + f.Slug, Repo: i.HomeRepo, Kind: KindFact, At: at,
			URL: "knowledge://" + string(f.Layer) + "/" + f.Slug, Title: f.Title,
			Text:   strings.TrimSpace(f.Body + "\n" + strings.Join(f.Tags, " ")),
			Fields: map[string]string{"type": string(f.Type), "layer": string(f.Layer), "status": f.Status, "state": string(f.State)},
		}
		i.put(r, key, &sourceEntry{stamp: stamp, first: at}, []Document{doc})
	}
}
