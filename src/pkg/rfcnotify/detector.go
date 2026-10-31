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
	// loads unchanged because every new field is optional.
	stateVersion = 2
	// closedRetention is how long a closed issue's record is kept so a
	// late re-read of the same close cannot post it twice.
	closedRetention = 30 * 24 * time.Hour
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
				events = append(events, Event{Kind: KindOpened, Issue: is})
			}
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
		d.state.Issues[key] = cur
	}
	for i := range events {
		events[i].Thread = events[i].Issue.Key()
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

func recordOf(is Issue, now time.Time) *issueRecord {
	return &issueRecord{
		Open:       is.Open(),
		Held:       !is.IsPR && is.Open() && Held(is.Labels),
		Actionable: !is.IsPR && Actionable(is.Labels),
		LastSeen:   now,
		PR:         is.IsPR,
	}
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
