package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ReviewLinksFile is the durable record of WHERE each hive review was posted.
//
// The review itself is already durable — it is a comment on GitHub — but
// nothing in the hive remembered its address, because CreateReview's response
// was discarded at the call site. That left the dashboard unable to answer the
// one question a maintainer looking at a queue actually asks: "has the hive
// looked at this one, and what did it say?" The audit trail records THAT a
// review happened, but an audit line is not a link, and re-deriving the URL
// would mean an API call per PR across a queue of hundreds.
//
// It lives on the durable data dir for the same reason the verdicts and
// dispatch state do (see review.ReviewVerdictsPath): under /var/run it sat on
// the container's ephemeral layer, and a spoke that restarts several times a
// day would lose the links faster than it accumulated them.
const ReviewLinksFile = "review-links.json"

// ReviewLinksDir is the durable data dir. Kept as its own constant rather than
// imported from pkg/review so this package stays independent of it — neither
// package imports the other today, and a link ledger is not worth reversing
// that.
const ReviewLinksDir = "/data"

// ReviewLinksPath is a var so tests can point it at a temp dir.
var ReviewLinksPath = filepath.Join(ReviewLinksDir, ReviewLinksFile)

// reviewLinksMu serializes the read-modify-write in RecordReviewLink. The
// review-request watcher processes one request at a time today, but the
// ledger is process-global state and a second writer would silently drop
// whichever update lost the race.
var reviewLinksMu sync.Mutex

// maxReviewLinks bounds the ledger. Entries are pruned oldest-first, so the
// most recent reviews — the ones a queue view is asking about — always
// survive. A hive governing twenty active repositories records a few hundred
// links a week, so this holds roughly a month of history in a file small
// enough to read on every snapshot.
const maxReviewLinks = 2000

// ReviewLink is where one review landed. State mirrors the review event the
// hive submitted ("commented", "approved", "changes_requested") so the pill
// can say what KIND of review it was without re-fetching it.
type ReviewLink struct {
	URL     string    `json:"url"`
	State   string    `json:"state,omitempty"`
	HeadSHA string    `json:"head_sha,omitempty"`
	At      time.Time `json:"at"`
	// Count is how many reviews the hive has posted on this PR in total. A
	// maintainer seeing "3" where they expected "1" is looking at the
	// duplicate-review problem directly, so it is worth carrying even though
	// URL points only at the most recent one.
	Count int `json:"count,omitempty"`
	// HeadCount is how many hive reviews have been posted at HeadSHA. It
	// resets to 1 when the head moves, so it is the number the per-head
	// backstop compares against: Count is lifetime and says nothing about
	// whether the CURRENT head has been judged.
	HeadCount int `json:"head_count,omitempty"`
}

type reviewLinkLedger struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Links       map[string]ReviewLink `json:"links"`
}

// ReviewLinkKey is the ledger key for a PR: "owner/repo#number".
func ReviewLinkKey(repo string, number int) string {
	return strings.TrimSpace(repo) + "#" + strconv.Itoa(number)
}

// LoadReviewLinks reads the ledger. A missing file is not an error — a hive
// that has never posted a review has no links, which is a legitimate state and
// must not be reported as a failure that the caller logs every cycle.
func LoadReviewLinks(path string) (map[string]ReviewLink, error) {
	if path == "" {
		path = ReviewLinksPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]ReviewLink{}, nil
		}
		return nil, err
	}
	var ledger reviewLinkLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if ledger.Links == nil {
		return map[string]ReviewLink{}, nil
	}
	return ledger.Links, nil
}

// RecordReviewLink stores where a review was posted, preserving the running
// count for that PR.
//
// A read-modify-write on every review is affordable because reviews are rare
// relative to everything else the hive does (tens per hour at most), and it
// keeps the ledger a plain artifact any operator can cat — the same tradeoff
// the verdict artifact makes.
func RecordReviewLink(path, repo string, number int, link ReviewLink) error {
	if path == "" {
		path = ReviewLinksPath
	}
	if strings.TrimSpace(link.URL) == "" {
		// A review with no URL is not a link. Recording it would put an entry
		// in the ledger that renders a pill pointing nowhere, which is worse
		// than no pill at all.
		return nil
	}
	reviewLinksMu.Lock()
	defer reviewLinksMu.Unlock()

	links, err := LoadReviewLinks(path)
	if err != nil {
		// A corrupt or unreadable ledger must not block the review that was
		// already posted to GitHub. Start a fresh one rather than failing.
		links = map[string]ReviewLink{}
	}
	key := ReviewLinkKey(repo, number)
	if link.At.IsZero() {
		link.At = time.Now().UTC()
	}
	link.At = link.At.UTC()
	if prev, ok := links[key]; ok {
		link.Count = prev.Count + 1
		if link.HeadSHA != "" && prev.HeadSHA == link.HeadSHA {
			link.HeadCount = prev.HeadCount + 1
		} else {
			link.HeadCount = 1
		}
	} else {
		link.Count = 1
		link.HeadCount = 1
	}
	links[key] = link
	pruneReviewLinks(links)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(reviewLinkLedger{
		GeneratedAt: time.Now().UTC(),
		Links:       links,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pruneReviewLinks drops the oldest entries once the ledger exceeds
// maxReviewLinks, in place.
func pruneReviewLinks(links map[string]ReviewLink) {
	if len(links) <= maxReviewLinks {
		return
	}
	keys := make([]string, 0, len(links))
	for k := range links {
		keys = append(keys, k)
	}
	// Oldest first; ties broken by key so pruning is deterministic and a test
	// cannot depend on map iteration order.
	sort.Slice(keys, func(i, j int) bool {
		a, b := links[keys[i]].At, links[keys[j]].At
		if a.Equal(b) {
			return keys[i] < keys[j]
		}
		return a.Before(b)
	})
	for _, k := range keys[:len(links)-maxReviewLinks] {
		delete(links, k)
	}
}
