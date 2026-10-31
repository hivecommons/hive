package rfcnotify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// stateVersion guards the on-disk shape. Version 2 adds the per-RFC
	// Discord mapping (issueRecord.MessageID and friends); a version 1 file
	// loads unchanged because every new field is optional. The lifecycle
	// fields (issueRecord.Tracked and friends) are optional too, so they
	// did not need a new version.
	stateVersion = 2
	// closedRetention is how long a closed issue's record is kept so a
	// late re-read of the same close cannot post it twice.
	closedRetention = 30 * 24 * time.Hour
	// decisionsDebounce is the shortest gap between two KindDecisions
	// notices for one RFC; changes inside it collapse into the next one.
	decisionsDebounce = 30 * time.Minute
	// duplicateWindow is how long after an RFC opens a same-author,
	// same-title RFC counts as its duplicate.
	duplicateWindow = time.Hour
)

// issueRecord is what the detector remembers about one RFC between polls.
type issueRecord struct {
	Open       bool `json:"open"`
	Held       bool `json:"held"`
	Actionable bool `json:"actionable"`
	// ReadyAnnounced is set once a ready notice went out for the current
	// hold episode; a new hold resets it.
	ReadyAnnounced bool      `json:"ready_announced,omitempty"`
	LastSeen       time.Time `json:"last_seen"`
	// PR marks an RFC document pull request.
	PR bool `json:"pr,omitempty"`

	// The fields below are the durable Discord mapping (threads mode). An
	// RFC announced before threads existed carries none of them and is never
	// back-filled with a thread.

	// AnnounceState is "" (no announcement owned by this hive), StatePending
	// (intent recorded, card not confirmed), StatePosted (card confirmed, no
	// thread) or StateThreaded.
	AnnounceState string `json:"announce_state,omitempty"`
	// Nonce is the deterministic Discord nonce of the opened card.
	Nonce string `json:"nonce,omitempty"`
	// MessageID is the Discord message of the opened card.
	MessageID string `json:"message_id,omitempty"`
	// ThreadID is the Discord thread started on that message.
	ThreadID string `json:"thread_id,omitempty"`
	// OpenText and ThreadName are what a retry re-posts and re-creates with.
	OpenText   string `json:"open_text,omitempty"`
	ThreadName string `json:"thread_name,omitempty"`
	// BacklinkPosted is set once the opt-in GitHub comment went out.
	BacklinkPosted bool `json:"backlink_posted,omitempty"`
	// PostTries, ThreadTries and BacklinkTries bound the retries of each step.
	PostTries     int `json:"post_tries,omitempty"`
	ThreadTries   int `json:"thread_tries,omitempty"`
	BacklinkTries int `json:"backlink_tries,omitempty"`

	// The fields below feed the rich-mode lifecycle kinds. Tracked marks a
	// record that carries them; one written before they existed is filled
	// on its next observation without announcing anything.
	Tracked bool `json:"tracked,omitempty"`
	// Title, URL, Author, CreatedAt and Private describe the issue for
	// retitle and duplicate detection and for a debounced decisions notice
	// sent while the issue itself is not in the poll.
	Title     string    `json:"title,omitempty"`
	URL       string    `json:"url,omitempty"`
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	Private   bool      `json:"private,omitempty"`
	// Target, Status and OpenDecisions are ParseRFC's view of the body.
	Target        string `json:"target,omitempty"`
	Status        string `json:"status,omitempty"`
	OpenDecisions int    `json:"open_decisions,omitempty"`
	// NeedsDecision mirrors the needs-decision / needs-human labels.
	NeedsDecision bool `json:"needs_decision,omitempty"`
	// DecisionsAnnounced is the open-decision count of the last decisions
	// notice (the baseline the next one is diffed from), and
	// DecisionsChangedAt is when that notice went out.
	DecisionsAnnounced int       `json:"decisions_announced,omitempty"`
	DecisionsChangedAt time.Time `json:"decisions_changed_at,omitempty"`
	// DuplicateOf is the key of the RFC this one duplicates; such a record
	// is never announced.
	DuplicateOf string `json:"duplicate_of,omitempty"`
	// Parsed is set once a body yielded RFC metadata. A snapshot whose body
	// does not keeps the previous Target, Status and OpenDecisions, and the
	// first one that does sets the baseline without announcing.
	Parsed bool `json:"parsed,omitempty"`
}

// Values of issueRecord.AnnounceState.
const (
	StatePending  = "pending"
	StatePosted   = "posted"
	StateThreaded = "threaded"
)

// inherit carries the Discord mapping of the previous record over to a
// record rebuilt from a fresh snapshot.
func (r *issueRecord) inherit(prev *issueRecord) {
	r.AnnounceState, r.Nonce, r.MessageID, r.ThreadID = prev.AnnounceState, prev.Nonce, prev.MessageID, prev.ThreadID
	r.OpenText, r.ThreadName, r.BacklinkPosted = prev.OpenText, prev.ThreadName, prev.BacklinkPosted
	r.PostTries, r.ThreadTries, r.BacklinkTries = prev.PostTries, prev.ThreadTries, prev.BacklinkTries
	r.DuplicateOf = prev.DuplicateOf
	if prev.Tracked && (prev.Parsed || !r.Parsed) {
		if !r.Parsed {
			r.Parsed, r.Target, r.Status, r.OpenDecisions = prev.Parsed, prev.Target, prev.Status, prev.OpenDecisions
		}
		r.DecisionsAnnounced, r.DecisionsChangedAt = prev.DecisionsAnnounced, prev.DecisionsChangedAt
	}
}

// State is the persisted detector memory.
type State struct {
	Version int `json:"version"`
	// Seeded is set after the first full read of open issues; before it,
	// nothing is announced because there is no prior to compare against.
	Seeded bool `json:"seeded"`
	// Watermark is when the last successful poll ran; the next poll asks
	// GitHub for issues updated since it (minus slack), and an unknown
	// issue created after it counts as newly opened.
	Watermark time.Time               `json:"watermark"`
	Issues    map[string]*issueRecord `json:"issues"`
}

// Detector turns successive issue snapshots into lifecycle events.
type Detector struct {
	state State
	// lifecycle enables the rich-mode kinds (decisions, target, status,
	// needs-decision, retitle, duplicate). Off, Observe returns exactly the
	// line-mode events.
	lifecycle bool
}

// NewDetector returns an empty, unseeded detector.
func NewDetector() *Detector {
	return &Detector{state: State{Version: stateVersion, Issues: map[string]*issueRecord{}}}
}

// Load reads a detector from path. A missing file yields an unseeded
// detector; a corrupt one is an error so the caller can decide, rather than
// silently re-announcing every RFC.
func Load(path string) (*Detector, error) {
	d := NewDetector()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read rfc notify state: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse rfc notify state: %w", err)
	}
	if st.Version > stateVersion {
		return nil, fmt.Errorf("rfc notify state %s is version %d, newer than the supported %d", path, st.Version, stateVersion)
	}
	if st.Issues == nil {
		st.Issues = map[string]*issueRecord{}
	}
	st.Version = stateVersion
	d.state = st
	return d, nil
}

// Save writes the state to path (0600, directory created as needed).
func (d *Detector) Save(path string) error { return writeState(path, d.state) }

// writeState persists st atomically.
func writeState(path string, st State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create rfc notify state dir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal rfc notify state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write rfc notify state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace rfc notify state: %w", err)
	}
	return nil
}

// Seeded reports whether the detector has a baseline to diff against.
func (d *Detector) Seeded() bool { return d.state.Seeded }

// Watermark is the time of the last successful poll (zero before seeding).
func (d *Detector) Watermark() time.Time { return d.state.Watermark }

// Known reports whether the detector has a record for the issue key.
func (d *Detector) Known(key string) bool { _, ok := d.state.Issues[key]; return ok }

// Seed records the current open issues (and any RFC pull requests passed
// along) without announcing anything and marks the detector seeded as of
// now. Call it once with the full open set.
func (d *Detector) Seed(issues []Issue, now time.Time) {
	for _, is := range issues {
		if !is.RFC() {
			continue
		}
		d.state.Issues[is.Key()] = recordOf(is, now)
	}
	d.state.Seeded = true
	d.state.Watermark = now
}

// Observe diffs issues (any issue updated since the last poll, open or
// closed) against the remembered records and returns the events to
// announce, in a stable order. It also advances the watermark to now and
// prunes closed records older than the retention window. Calling it with
// the same snapshot twice yields events only once.
//
// Pull requests (Issue.IsPR) that propose an RFC document yield KindOpened
// when opened after the watermark and KindDocAccepted when merged; a PR
// closed without merging yields nothing.
func (d *Detector) Observe(issues []Issue, now time.Time) []Event {
	if !d.state.Seeded {
		d.Seed(issues, now)
		return nil
	}
	if d.lifecycle {
		// Oldest first, so of two identical RFCs in one poll the earlier one
		// is the original and the later one its duplicate.
		issues = append([]Issue(nil), issues...)
		sort.SliceStable(issues, func(i, j int) bool { return issues[i].CreatedAt.Before(issues[j].CreatedAt) })
	}
	var events []Event
	for _, is := range issues {
		if !is.RFC() {
			// An issue that stopped being an RFC (label dropped, title
			// edited) is forgotten so a later re-labeling starts clean.
			delete(d.state.Issues, is.Key())
			continue
		}
		key := is.Key()
		prev, known := d.state.Issues[key]
		cur := recordOf(is, now)
		if known {
			cur.inherit(prev)
		}
		switch {
		case is.IsPR:
			if ev, ok := d.observePR(is, prev, known); ok {
				events = append(events, ev)
			}
		case !known:
			if is.Open() && is.CreatedAt.After(d.state.Watermark) {
				if orig := d.duplicateOf(is); orig != "" {
					cur.DuplicateOf = orig
					events = append(events, Event{Kind: KindDuplicate, Issue: is, Thread: orig})
				} else {
					events = append(events, Event{Kind: KindOpened, Issue: is})
				}
			}
		case prev.Open && !cur.Open && d.lifecycle && prev.DuplicateOf != "":
			// A duplicate was never announced, so its close is not either.
		case prev.Open && !cur.Open:
			events = append(events, Event{Kind: KindClosed, Issue: is})
		case prev.Open && cur.Open && prev.Held:
			// Inside a hold episode. One ready notice per episode: the
			// flag rides along while the hold label stays on and resets
			// once it is gone, so the next hold starts clean.
			lifted := !cur.Held
			staged := cur.Actionable && !prev.Actionable
			if !prev.ReadyAnnounced && (lifted || staged) {
				events = append(events, Event{Kind: KindReady, Issue: is})
				cur.ReadyAnnounced = true
			} else if cur.Held {
				cur.ReadyAnnounced = prev.ReadyAnnounced
			}
		}
		if d.lifecycle && known && !is.IsPR && prev.Tracked && prev.Open && cur.Open && cur.DuplicateOf == "" {
			events = append(events, changes(is, prev, cur)...)
		}
		if !d.lifecycle {
			// Line mode never announces decisions, so nothing is pending.
			cur.DecisionsAnnounced = cur.OpenDecisions
		}
		d.state.Issues[key] = cur
	}
	if d.lifecycle {
		events = append(events, d.decisions(now)...)
	}
	for i := range events {
		if events[i].Thread == "" {
			events[i].Thread = events[i].Issue.Key()
		}
		if events[i].Kind == KindDocAccepted {
			d.linkThread(&events[i])
		}
	}
	d.state.Watermark = now
	d.prune(now)
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i].Issue, events[j].Issue
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Number < b.Number
	})
	return events
}

// observePR decides the event for an RFC pull request. Like issues, an
// unknown PR is announced only when it was opened after the watermark (so
// proposals already open at seed time stay silent); a merge is announced
// once, when seen after the watermark or as an open record closing.
func (d *Detector) observePR(is Issue, prev *issueRecord, known bool) (Event, bool) {
	switch {
	case !known && is.Open():
		if is.CreatedAt.After(d.state.Watermark) {
			return Event{Kind: KindOpened, Issue: is}, true
		}
	case !known && is.Merged():
		if is.MergedAt.After(d.state.Watermark) {
			return Event{Kind: KindDocAccepted, Issue: is}, true
		}
	case known && prev.Open && !is.Open() && is.Merged():
		return Event{Kind: KindDocAccepted, Issue: is}, true
	}
	return Event{}, false
}

// linkThread points a merged RFC doc PR at the thread of the RFC issue its
// body links (Closes/Refs #N or an issue URL), when the detector tracks
// that issue as an RFC; otherwise the PR's own thread is kept.
func (d *Detector) linkThread(ev *Event) {
	for _, key := range LinkedIssueKeys(ev.Issue.Repo, ev.Issue.Body) {
		rec, ok := d.state.Issues[key]
		if !ok || rec.PR {
			continue
		}
		_, num, _ := strings.Cut(key, "#")
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		ev.Thread, ev.LinkedRFC = key, n
		return
	}
}

// changes returns the target, status, needs-decision and retitle events of
// an open RFC between two observations. Target and status changes are
// announced only between two known values, so a body that stopped parsing
// is not a change.
func changes(is Issue, prev, cur *issueRecord) []Event {
	var out []Event
	if prev.Title != "" && cur.Title != prev.Title {
		out = append(out, Event{Kind: KindRetitled, Issue: is, From: prev.Title, To: cur.Title})
	}
	if prev.Target != "" && cur.Target != "" && !strings.EqualFold(prev.Target, cur.Target) {
		out = append(out, Event{Kind: KindTargetChanged, Issue: is, From: prev.Target, To: cur.Target})
	}
	if prev.Status != "" && cur.Status != "" && !strings.EqualFold(prev.Status, cur.Status) {
		out = append(out, Event{Kind: KindStatusChanged, Issue: is, From: prev.Status, To: cur.Status})
	}
	if cur.NeedsDecision && !prev.NeedsDecision {
		out = append(out, Event{Kind: KindNeedsDecision, Issue: is})
	}
	return out
}

// decisions returns one KindDecisions per open RFC whose open-decision count
// differs from the last announced one and whose debounce window has passed,
// whether or not the issue was in this poll, and moves the baseline.
func (d *Detector) decisions(now time.Time) []Event {
	var keys []string
	for key, rec := range d.state.Issues {
		if rec.Tracked && rec.Open && !rec.PR && rec.DuplicateOf == "" && rec.OpenDecisions != rec.DecisionsAnnounced &&
			(rec.DecisionsChangedAt.IsZero() || now.Sub(rec.DecisionsChangedAt) >= decisionsDebounce) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]Event, 0, len(keys))
	for _, key := range keys {
		rec := d.state.Issues[key]
		repo, num, _ := strings.Cut(key, "#")
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		is := Issue{Repo: repo, Number: n, Title: rec.Title, URL: rec.URL, Author: rec.Author, State: "open", CreatedAt: rec.CreatedAt, Private: rec.Private}
		out = append(out, Event{Kind: KindDecisions, Issue: is, From: strconv.Itoa(rec.DecisionsAnnounced), To: strconv.Itoa(rec.OpenDecisions)})
		rec.DecisionsAnnounced, rec.DecisionsChangedAt = rec.OpenDecisions, now
	}
	return out
}

// duplicateOf returns the key of an open, announced RFC in the same
// repository that the same author opened with the same normalised title at
// most duplicateWindow before is, or "". Only in rich mode.
func (d *Detector) duplicateOf(is Issue) string {
	if !d.lifecycle || is.IsPR {
		return ""
	}
	title, author := NormalizeTitle(is.Title), strings.TrimSpace(is.Author)
	if title == "" || author == "" {
		return ""
	}
	best := ""
	var bestAt time.Time
	for key, rec := range d.state.Issues {
		if rec.PR || !rec.Open || rec.DuplicateOf != "" || rec.CreatedAt.IsZero() || key == is.Key() {
			continue
		}
		if !strings.HasPrefix(key, is.Repo+"#") || !strings.EqualFold(rec.Author, author) || NormalizeTitle(rec.Title) != title {
			continue
		}
		if age := is.CreatedAt.Sub(rec.CreatedAt); age < 0 || age > duplicateWindow {
			continue
		}
		if best == "" || rec.CreatedAt.Before(bestAt) || (rec.CreatedAt.Equal(bestAt) && key < best) {
			best, bestAt = key, rec.CreatedAt
		}
	}
	return best
}

func recordOf(is Issue, now time.Time) *issueRecord {
	r := &issueRecord{
		Open:       is.Open(),
		Held:       !is.IsPR && is.Open() && Held(is.Labels),
		Actionable: !is.IsPR && Actionable(is.Labels),
		LastSeen:   now,
		PR:         is.IsPR,

		Tracked:   true,
		Title:     strings.TrimSpace(is.Title),
		URL:       strings.TrimSpace(is.URL),
		Author:    strings.TrimSpace(is.Author),
		CreatedAt: is.CreatedAt,
		Private:   is.Private || is.VisibilityUnknown,
	}
	if !is.IsPR {
		r.NeedsDecision = NeedsDecision(is.Labels)
		if meta := ParseRFC(is.Body); !meta.Empty() {
			r.Parsed = true
			r.Target, r.Status, r.OpenDecisions = strings.TrimSpace(meta.Target), strings.TrimSpace(meta.Status), meta.OpenDecisions
		}
		r.DecisionsAnnounced = r.OpenDecisions
	}
	return r
}

func (d *Detector) prune(now time.Time) {
	for key, rec := range d.state.Issues {
		if !rec.Open && now.Sub(rec.LastSeen) > closedRetention {
			delete(d.state.Issues, key)
		}
	}
}

// clone copies the state so an Observe can be partially undone.
func (d *Detector) clone() State { return cloneState(d.state) }

func cloneState(st State) State {
	out := st
	out.Issues = make(map[string]*issueRecord, len(st.Issues))
	for k, v := range st.Issues {
		rec := *v
		out.Issues[k] = &rec
	}
	return out
}

// checkpoint persists base with key's current record folded in. base is the
// pre-poll state advanced only by what has been delivered so far (same
// watermark as before the poll), so a crash mid-batch re-observes every
// event not yet delivered while keeping the write-ahead intent and the
// mapping of the ones that were.
func (d *Detector) checkpoint(path string, base *State, key string) error {
	if path == "" {
		return nil
	}
	if rec, ok := d.state.Issues[key]; ok {
		cp := *rec
		base.Issues[key] = &cp
	}
	return writeState(path, *base)
}

// rollback restores the records of the issues behind unposted events to
// what they were in before, and moves the watermark back so the next poll
// lists them again. Records of issues whose events were posted are kept.
func (d *Detector) rollback(before State, unposted []Event) {
	for _, ev := range unposted {
		key := ev.Issue.Key()
		if rec, ok := before.Issues[key]; ok {
			cp := *rec
			d.state.Issues[key] = &cp
		} else {
			delete(d.state.Issues, key)
		}
	}
	d.state.Watermark = before.Watermark
}
