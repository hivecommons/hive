package dashboard

import (
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageTrackerLineage describes the provenance of the live copy, independently
// of whether the original is still linked, closed, or its copy has merged.
type StageTrackerLineage struct {
	Kind           string     `json:"kind"`
	FromNumber     int        `json:"from_number"`
	FromURL        string     `json:"from_url"`
	FromAuthor     string     `json:"from_author,omitempty"`
	FromAuthorKind string     `json:"from_author_kind,omitempty"`
	ToNumber       int        `json:"to_number"`
	ToURL          string     `json:"to_url"`
	At             *time.Time `json:"at,omitempty"`
	Reason         string     `json:"reason,omitempty"`
}

func stageTrackerLineageIndex(store *stagelabels.Store, baseURL string) map[string]StageTrackerLineage {
	out := map[string]StageTrackerLineage{}
	if store == nil {
		return out
	}
	for _, key := range store.Keys() {
		rec, ok := store.Get(key)
		if !ok || rec.AdoptedBy <= 0 {
			continue
		}
		repo, text, found := strings.Cut(key, "#")
		number, err := strconv.Atoi(text)
		if !found || err != nil || repo == "" || number <= 0 || number == rec.AdoptedBy {
			continue
		}
		by := stagelabels.Key(repo, rec.AdoptedBy)
		if prev, exists := out[by]; exists && prev.FromNumber <= number {
			continue
		}
		l := StageTrackerLineage{Kind: "adopted", FromNumber: number, FromURL: overviewItemURL("", repo, number, "pr", baseURL), FromAuthor: rec.Author, ToNumber: rec.AdoptedBy, ToURL: overviewItemURL("", repo, rec.AdoptedBy, "pr", baseURL), Reason: rec.AdoptReason}
		if !rec.AdoptedAt.IsZero() {
			at := rec.AdoptedAt
			l.At = &at
		}
		out[by] = l
	}
	return out
}

// No network lookup: an App-owned adoption branch is enough to retain the
// original reference when the stage record has expired. Unknown facts stay empty.
func stageTrackerPRLineage(index map[string]StageTrackerLineage, repo string, pr github.PullRequest, baseURL string) *StageTrackerLineage {
	if l, ok := index[stagelabels.Key(repo, pr.Number)]; ok {
		return &l
	}
	if !pr.AppAuthored || !strings.HasPrefix(pr.HeadRef, "hive/adopt-") {
		return nil
	}
	number, err := strconv.Atoi(strings.TrimPrefix(pr.HeadRef, "hive/adopt-"))
	if err != nil || number <= 0 || number == pr.Number {
		return nil
	}
	return &StageTrackerLineage{Kind: "adopted", FromNumber: number, FromURL: overviewItemURL("", repo, number, "pr", baseURL), ToNumber: pr.Number, ToURL: overviewItemURL(pr.URL, repo, pr.Number, "pr", baseURL)}
}
