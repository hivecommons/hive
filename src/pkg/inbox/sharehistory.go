package inbox

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Share destinations as the history records them.
const (
	ShareDestRFC       = "rfc"
	ShareDestDibs      = "dibs"
	ShareDestKnowledge = "knowledge"
	ShareDestSpek      = "spek"
)

// Share record statuses.
const (
	ShareQueued       = "queued"
	ShareTranscribing = "transcribing"
	ShareInProgress   = "in_progress"
	ShareDone         = "done"
	ShareFailed       = "failed"
)

const (
	// AutoRouteActor is the actor of shares made by auto-routing rules.
	AutoRouteActor = "inbox-auto"
	// maxShareRecords caps one item's history (oldest dropped first).
	maxShareRecords = 200
	// maxShareReasonRunes bounds a record's reason text.
	maxShareReasonRunes = 300
)

// ShareRecord is one entry of an item's append-only share history: what was
// shared, by whom, where, when, and how it went. It holds metadata only,
// never item text or secrets.
type ShareRecord struct {
	ID          string     `json:"id"`
	Destination string     `json:"destination"`
	Actor       string     `json:"actor"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	Ref         *ShareRef  `json:"ref,omitempty"`
}

// ShareRef points at what the share produced.
type ShareRef struct {
	// URL is the RFC issue or PR, the dibs item or a link to the fact.
	URL string `json:"url,omitempty"`
	// Number is the RFC issue or PR number.
	Number int `json:"number,omitempty"`
	// ID is the dibs item id, the knowledge fact id or the spek session id.
	ID string `json:"id,omitempty"`
	// Name is the spek session name (or the fact layer).
	Name string `json:"name,omitempty"`
}

// shareDestFor maps a route destination to its history name.
func shareDestFor(dest string) string {
	switch dest {
	case DestSpekRFC:
		return ShareDestRFC
	case DestDismiss:
		return ShareDestKnowledge
	}
	return dest
}

var issueNumberRe = regexp.MustCompile(`/(?:issues|pull)/(\d+)(?:[/?#].*)?$`)

// refFromResult builds the target ref of a finished route.
func refFromResult(it Item, dest string, res Result) *ShareRef {
	switch dest {
	case DestSpekRFC:
		if res.URL == "" {
			return nil
		}
		ref := &ShareRef{URL: res.URL}
		if m := issueNumberRe.FindStringSubmatch(res.URL); m != nil {
			ref.Number, _ = strconv.Atoi(m[1])
		}
		return ref
	case DestDibs:
		if res.URL == "" {
			return nil
		}
		ref := &ShareRef{URL: res.URL}
		if i := strings.LastIndex(res.URL, "/"); i >= 0 && i+1 < len(res.URL) {
			ref.ID = res.URL[i+1:]
		}
		return ref
	case DestDismiss:
		if it.FactSlug == "" {
			return nil
		}
		return &ShareRef{ID: it.FactSlug, Name: it.Layer}
	}
	return nil
}

func clipReason(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxShareReasonRunes {
		return string(r[:maxShareReasonRunes]) + "…"
	}
	return s
}

// RecordShare appends a share record to the item's history and returns its
// id. It is the helper other destinations (the spek session seed) call when
// they start a share; finish it with FinishShare. StartedAt and Status
// default to now and queued.
func (s *Service) RecordShare(key string, rec ShareRecord) (string, error) {
	if rec.StartedAt.IsZero() {
		rec.StartedAt = s.now()
	}
	if rec.Status == "" {
		rec.Status = ShareQueued
	}
	rec.Reason = clipReason(rec.Reason)
	return s.Store.AppendShare(key, rec)
}

// FinishShare moves a record to status (in_progress, done or failed) with an
// optional reason and target ref. done and failed set FinishedAt.
func (s *Service) FinishShare(key, id, status, reason string, ref *ShareRef) error {
	return s.Store.UpdateShare(key, id, func(r *ShareRecord) {
		r.Status, r.Reason = status, clipReason(reason)
		if ref != nil {
			c := *ref
			r.Ref = &c
		}
		if status == ShareDone || status == ShareFailed {
			t := s.now()
			r.FinishedAt = &t
		}
	})
}

// RecordRFCPublished fills the RFC target of a delegated share once the
// agent's spek publish output is known: the route becomes filed with the RFC
// URL and children, and the share record becomes done with the issue number.
func (s *Service) RecordRFCPublished(source, id, publishOutput string) error {
	res, err := ParsePublishOutput(publishOutput)
	if err != nil {
		return err
	}
	key := Key(source, id)
	if _, err := s.Store.Set(key, DestSpekRFC, StatusFiled, res, ""); err != nil {
		return err
	}
	ref := refFromResult(Item{}, DestSpekRFC, res)
	return s.Store.FinishLatestShare(key, ShareDestRFC, func(r *ShareRecord) {
		r.Status, r.Reason, r.Ref = ShareDone, "", ref
		t := s.now()
		r.FinishedAt = &t
	})
}

func cloneHistory(in []*ShareRecord) []*ShareRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]*ShareRecord, len(in))
	for i, r := range in {
		c := *r
		if r.Ref != nil {
			ref := *r.Ref
			c.Ref = &ref
		}
		if r.FinishedAt != nil {
			t := *r.FinishedAt
			c.FinishedAt = &t
		}
		out[i] = &c
	}
	return out
}
