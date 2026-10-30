package dashboard

import (
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Lineage on a tracker row (hivecommons/hive: issue #15198 showed PR
// #15308 "Fixing" with nothing saying #15308 was the hive's copy of
// @thatguywob's #15226, adopted after CI went red and the contributor's
// agent stayed silent — so routing the fix to a hive agent read as the
// hive taking over a clanker's PR for no reason). The superseding PR's
// row now carries `lineage`: where it came from, whose it was, when and
// why; the closed original (when it is still rendered, close_first off)
// carries the reverse. Everything is read from the stage store's adoption
// record (MarkAdopted / MarkSupersededClosedFirst — the same facts the
// "Supersedes #N" body line was written from) or, with no record, from the
// hive/adopt-<n> head branch. Nothing is fetched from GitHub.

// Lineage kinds.
const (
	lineageAdopted       = "adopted"
	lineageSuperseded    = "superseded"
	lineageRebasedByHive = "rebased-by-hive"
)

// Lineage from_state values.
const (
	lineageFromStateOpen   = "open"
	lineageFromStateClosed = "closed"
)

// Lineage reason slugs the SPA words its hover from.
const (
	lineageReasonCIRedOffline      = "ci-red-contributor-offline"
	lineageReasonCIRedUnregistered = "ci-red-contributor-unregistered"
	lineageReasonOperatorAsked     = "operator-asked"
)

// StageTrackerLineage is the optional `lineage` field of a PR row: on the
// hive's copy, kind adopted / rebased-by-hive with the original in
// from_*; on the original, kind superseded with the copy in to_*.
type StageTrackerLineage struct {
	Kind string `json:"kind"`
	// FromNumber / FromURL / FromAuthor / FromAuthorKind name the original
	// PR on the hive's copy (author kind human / clanker / hive, "" when
	// unknown).
	FromNumber     int    `json:"from_number,omitempty"`
	FromURL        string `json:"from_url,omitempty"`
	FromAuthor     string `json:"from_author,omitempty"`
	FromAuthorKind string `json:"from_author_kind,omitempty"`
	// FromState is the original's state on the hive's copy: "closed" once
	// the hive closed it as superseded, "open" while it is still open
	// (it closes when the copy merges), "" when unknown. The copy's chip
	// is the only place the superseded original is drawn
	// (hivecommons/hive#15526), so its hover says which.
	FromState string `json:"from_state,omitempty"`
	// ToNumber / ToURL name the hive's copy on the original.
	ToNumber int    `json:"to_number,omitempty"`
	ToURL    string `json:"to_url,omitempty"`
	// At is when the hive adopted; Reason a short slug
	// (ci-red-contributor-offline, operator-asked, dco-rehome, …).
	At     *time.Time `json:"at,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// stageTrackerLineageReason folds the adoption record's AdoptReason into
// the lineage slug. An empty reason is the oldest path: CI red, nudged,
// contributor offline.
func stageTrackerLineageReason(adoptReason string) string {
	switch adoptReason {
	case "", automerge.AdoptReasonOffline, automerge.AdoptReasonGrace:
		return lineageReasonCIRedOffline
	case automerge.AdoptReasonUnregistered:
		return lineageReasonCIRedUnregistered
	case automerge.AdoptReasonOperator, automerge.AdoptReasonOperatorForced:
		return lineageReasonOperatorAsked
	}
	return adoptReason
}

// stageTrackerLineageAuthorKind guesses the original author's kind when
// the original is not a row any more: an adoption after a relay went
// offline means a registered relay existed (clanker); an unregistered
// author has no agent here (human). "" when the reason says nothing.
func stageTrackerLineageAuthorKind(adoptReason string) string {
	switch adoptReason {
	case "", automerge.AdoptReasonOffline, automerge.AdoptReasonGrace:
		return authorKindClanker
	case automerge.AdoptReasonUnregistered:
		return authorKindHuman
	}
	return ""
}

// stageTrackerLineageIndex maps store keys to lineage: the hive's copy
// (adopted / rebased-by-hive, from the original's AdoptedBy or
// SupersededBy) and the original (superseded). In-place adoptions name no
// second PR and get no lineage. When several originals name the same copy
// the lowest-numbered one wins.
func stageTrackerLineageIndex(store *stagelabels.Store, baseURL string) map[string]StageTrackerLineage {
	out := map[string]StageTrackerLineage{}
	if store == nil {
		return out
	}
	for _, key := range store.Keys() {
		rec, ok := store.Get(key)
		if !ok {
			continue
		}
		by := rec.SupersededBy
		if by <= 0 {
			if rec.AdoptMode == stagelabels.AdoptModeInPlace {
				continue
			}
			by = rec.AdoptedBy
		}
		if by <= 0 {
			continue
		}
		repo, numText, found := strings.Cut(key, "#")
		number, err := strconv.Atoi(numText)
		if !found || err != nil || number <= 0 || repo == "" {
			continue
		}
		copyKey := stagelabels.Key(repo, by)
		if prev, dup := out[copyKey]; dup && prev.FromNumber > 0 && prev.FromNumber <= number {
			continue
		}
		kind := lineageAdopted
		if rec.AdoptReason == automerge.AdoptReasonRebaseRehome {
			kind = lineageRebasedByHive
		}
		var at *time.Time
		for _, t := range []time.Time{rec.AdoptedAt, rec.SupersededAt} {
			if !t.IsZero() {
				tt := t
				at = &tt
				break
			}
		}
		reason := stageTrackerLineageReason(rec.AdoptReason)
		fromState := ""
		if rec.Superseded() && !rec.SupersededOpen {
			fromState = lineageFromStateClosed
		}
		out[copyKey] = StageTrackerLineage{Kind: kind, FromNumber: number, FromURL: overviewItemURL("", repo, number, "pr", baseURL),
			FromAuthor: rec.Author, FromAuthorKind: stageTrackerLineageAuthorKind(rec.AdoptReason), FromState: fromState, At: at, Reason: reason}
		out[key] = StageTrackerLineage{Kind: lineageSuperseded, ToNumber: by, ToURL: overviewItemURL("", repo, by, "pr", baseURL), At: at, Reason: reason}
	}
	return out
}

// stageTrackerLineageFromHead reads the original's number off a
// hive/adopt-<n> head branch, for a copy whose store record is gone.
func stageTrackerLineageFromHead(repo, headRef, baseURL string) *StageTrackerLineage {
	rest, ok := strings.CutPrefix(headRef, "hive/adopt-")
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return nil
	}
	return &StageTrackerLineage{Kind: lineageAdopted, FromNumber: n, FromURL: overviewItemURL("", repo, n, "pr", baseURL), Reason: lineageReasonCIRedOffline}
}

// stageTrackerLineageFor is the lineage of repo#number from the index,
// else from its head branch; nil when it has none.
func stageTrackerLineageFor(idx map[string]StageTrackerLineage, repo string, number int, headRef, baseURL string) *StageTrackerLineage {
	if l, ok := idx[stagelabels.Key(repo, number)]; ok {
		return &l
	}
	return stageTrackerLineageFromHead(repo, headRef, baseURL)
}

// stageTrackerLineageFillAuthors completes from_author / from_author_kind
// on each copy whose original is itself a row (close_first off, or the
// write-back gate kept it open): the row's own author fields beat the
// record's guess, and from_state reads open.
func stageTrackerLineageFillAuthors(items []StageTrackerItem) {
	prs := map[string]*StageTrackerItem{}
	for i := range items {
		if items[i].Kind == "pr" {
			prs[stagelabels.Key(items[i].Repo, items[i].Number)] = &items[i]
		}
	}
	for i := range items {
		l := items[i].Lineage
		if l == nil || l.FromNumber <= 0 {
			continue
		}
		orig := prs[stagelabels.Key(items[i].Repo, l.FromNumber)]
		if orig == nil {
			continue
		}
		if orig.Author != "" {
			l.FromAuthor = orig.Author
		}
		if orig.AuthorKind != "" {
			l.FromAuthorKind = orig.AuthorKind
		}
		l.FromState = lineageFromStateOpen
	}
}
