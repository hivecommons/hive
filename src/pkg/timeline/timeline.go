// Package timeline captures issue→PR lifecycle signals into an in-memory,
// bounded, concurrency-safe set of JOURNEYS the dashboard can render as a
// lifecycle view (issue enumerated → classified → kicked → PR opened →
// merged/blocked).
//
// It is deliberately decoupled from the tracing package: the timeline never
// imports OTel or governor internals. Instead, whatever produces lifecycle
// signals (the governor eval loop, the scheduler's classifier, the attribution
// audit sink, the escalation sweep, tests) simply hands the Store an Event via
// the Recorder interface. A tiny FromSpan helper translates a span name +
// attribute map into an Event so a span-processor adapter elsewhere can stay a
// thin shim.
//
// Identity and dedupe (#5656): events are keyed by IssueRef + Kind. Recording
// the same (ref, kind) again refreshes the existing stage (LastAt, Count,
// attrs) on that item's Journey instead of appending a new row, so the
// re-enumeration sweep that runs every governor eval cycle refreshes
// timestamps rather than flooding the ring and evicting real merge/block
// outcomes. The Store is bounded by a journey count (MaxJourneys), guarded by
// a mutex, and optionally persisted to a JSON file with the repo's
// atomic-persist idiom (see EnablePersistence) so restarts don't wipe it.
package timeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxJourneys is the default capacity of a Store: the maximum number of
// distinct issue/PR journeys retained. Once full, each new journey evicts the
// least-recently-touched one. Journeys (one per work item) are far denser than
// raw events, so 500 journeys spans weeks of fleet activity where the old
// 500-raw-event ring spanned ~14 minutes on a busy spoke (#5656).
const MaxJourneys = 500

// DefaultFleetWindow is the look-back window FleetHealth uses when the caller
// passes a non-positive duration. It bounds "recent" derived counts to a
// rolling window rather than the entire retained history.
const DefaultFleetWindow = 6 * time.Hour

// Kind enumerates the lifecycle stages an Event can represent. The set is
// closed and ordered along the issue→PR journey.
type Kind string

const (
	// KindEnumerated: the issue was discovered/enumerated by the governor.
	KindEnumerated Kind = "enumerated"
	// KindClassified: the issue was triaged/classified (lane, tier, model).
	KindClassified Kind = "classified"
	// KindKicked: an agent was kicked to work the issue.
	KindKicked Kind = "kicked"
	// KindPROpened: a pull request was opened for the issue.
	KindPROpened Kind = "pr_opened"
	// KindMerged: the pull request merged (terminal, success).
	KindMerged Kind = "merged"
	// KindBlocked: the issue/PR is blocked (needs attention). Not strictly
	// terminal: progress recorded after the block clears it.
	KindBlocked Kind = "blocked"
	// KindStageCompleted: a long-running run lease stage advanced/retried.
	KindStageCompleted Kind = "stage_completed"
	// KindStageReceipt records the stage receipt the Spektacular runner wrote
	// when an artifact reached final; attrs carry the receipt digest and path
	// (hivecommons/hive#8303).
	KindStageReceipt Kind = "stage_receipt"
	// KindStageApproval records an approval that released a held run stage.
	KindStageApproval Kind = "stage_approval"
)

// progressKinds are the non-terminal stages in furthest-first order, used to
// derive a Journey's current stage.
var progressKinds = []Kind{KindPROpened, KindStageCompleted, KindKicked, KindClassified, KindEnumerated}

// Event is a single lifecycle datapoint. It is a plain value type with JSON
// tags so producers and the FromSpan shim can stay decoupled from the Store's
// journey representation.
type Event struct {
	// ID is a stable identifier for the event. Dedupe no longer needs it
	// (identity is IssueRef+Kind); it is kept for producers that anchor events
	// and for the synthesized views (Recent/ByIssue).
	ID string `json:"id"`
	// IssueRef identifies the work item, e.g. "repo#123". Events with an empty
	// ref have no journey identity and are dropped by the Store.
	IssueRef string `json:"issueRef"`
	// Kind is the lifecycle stage. See the Kind constants.
	Kind Kind `json:"kind"`
	// Agent is the agent associated with the event, when applicable.
	Agent string `json:"agent,omitempty"`
	// At is the event time in Unix milliseconds.
	At int64 `json:"at"`
	// Attrs carries free-form structured context (labels, PR number, reason).
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Recorder is the narrow write side of the timeline. Producers depend only on
// this interface, keeping the timeline decoupled and trivially mockable.
type Recorder interface {
	Record(Event)
}

// Stage is one lifecycle stage's aggregate on a Journey: when it first and
// last fired, how many times, and the latest agent/attrs seen for it.
type Stage struct {
	// FirstAt / LastAt are Unix milliseconds of the first and most recent
	// recording of this stage. Re-recording refreshes LastAt.
	FirstAt int64 `json:"firstAt"`
	LastAt  int64 `json:"lastAt"`
	// Count is how many times this stage was recorded (e.g. kicks received).
	Count int `json:"count"`
	// Agent is the most recent non-empty agent recorded for this stage.
	Agent string `json:"agent,omitempty"`
	// Attrs merges the attrs of every recording, newest value per key winning.
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Journey is the full lifecycle of one work item: which stages it has reached,
// when, and where it currently stands. One Journey per IssueRef.
type Journey struct {
	// Ref is the work-item identity, e.g. "repo#123".
	Ref string `json:"ref"`
	// Agent is the most recent non-empty agent seen on any stage.
	Agent string `json:"agent,omitempty"`
	// Current is the derived current stage: merged if ever merged; blocked if
	// blocked with no later progress; otherwise the furthest progress stage.
	Current Kind `json:"current"`
	// FirstAt / LastAt bound the journey's activity, Unix milliseconds.
	FirstAt int64 `json:"firstAt"`
	LastAt  int64 `json:"lastAt"`
	// Stages holds the per-stage aggregates, keyed by Kind.
	Stages map[Kind]*Stage `json:"stages"`
	// Details is an additive dashboard payload derived at snapshot time. It is
	// omitted from persistence and old consumers can ignore it.
	Details *JourneyDetails `json:"details,omitempty"`
}

// JourneyDetails is the expanded, read-only view a dashboard row can render
// without reinterpreting stage attrs in JavaScript.
type JourneyDetails struct {
	Stages             []StageDetail       `json:"stages"`
	Agent              string              `json:"agent,omitempty"`
	Classification     *ClassificationInfo `json:"classification,omitempty"`
	Links              []JourneyLink       `json:"links,omitempty"`
	BlockedReason      string              `json:"blockedReason,omitempty"`
	TimeInCurrentStage int64               `json:"timeInCurrentStageMs,omitempty"`
	KickIDs            []string            `json:"kickIds,omitempty"`
	RunLogIDs          []string            `json:"runLogIds,omitempty"`
	Attrs              map[string]string   `json:"attrs,omitempty"`
}

// StageDetail describes one stage in chronological order plus the elapsed
// time since the previous recorded stage.
type StageDetail struct {
	Kind                    Kind              `json:"kind"`
	Label                   string            `json:"label"`
	FirstAt                 int64             `json:"firstAt"`
	LastAt                  int64             `json:"lastAt"`
	Count                   int               `json:"count"`
	Agent                   string            `json:"agent,omitempty"`
	Attrs                   map[string]string `json:"attrs,omitempty"`
	DurationSincePreviousMs int64             `json:"durationSincePreviousMs,omitempty"`
}

// ClassificationInfo summarizes the classifier attrs when present.
type ClassificationInfo struct {
	Band   string `json:"band,omitempty"`
	Lane   string `json:"lane,omitempty"`
	Tier   string `json:"tier,omitempty"`
	Model  string `json:"model,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// JourneyLink is a safe external link associated with the work item.
type JourneyLink struct {
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	URL    string `json:"url"`
	Number string `json:"number,omitempty"`
}

// clone returns a deep copy so accessors never leak shared maps.
func (j *Journey) clone() Journey {
	cp := *j
	cp.Details = nil
	cp.Stages = make(map[Kind]*Stage, len(j.Stages))
	for k, st := range j.Stages {
		s := *st
		if st.Attrs != nil {
			s.Attrs = make(map[string]string, len(st.Attrs))
			for ak, av := range st.Attrs {
				s.Attrs[ak] = av
			}
		}
		cp.Stages[k] = &s
	}
	return cp
}

// persistMinInterval throttles journey persistence: floods of non-terminal
// refreshes (the per-cycle enumeration sweep) coalesce into at most one write
// per interval, while terminal-ish stages (pr_opened/merged/blocked) persist
// immediately because losing one to a crash would silently zero the fleet
// counters the panel exists to show. Dirty state left behind by the throttle
// is flushed by the next Record after the interval — the governor eval cycle
// guarantees one lands within eval_interval_s.
const persistMinInterval = 15 * time.Second

// persistErrLogInterval throttles save-failure logging so a full disk does not
// turn every enumeration sweep into a log flood.
const persistErrLogInterval = time.Minute

// timelineFileMode matches the outcome/proof/beads/ledger persistence idiom.
const timelineFileMode = 0o660

// timelineFormatVersion is the persisted schema version.
const timelineFormatVersion = 1

// persistedTimeline is the on-disk shape.
type persistedTimeline struct {
	Version  int       `json:"version"`
	Journeys []Journey `json:"journeys"`
}

// Store is a bounded, concurrency-safe set of lifecycle Journeys keyed by
// IssueRef, with optional file persistence.
//
// The zero value is NOT ready for use — construct with NewStore.
type Store struct {
	mu       sync.RWMutex
	journeys map[string]*Journey
	cap      int

	// persistence (all guarded by mu)
	path        string
	logger      *slog.Logger
	dirty       bool
	lastPersist time.Time
	lastErrLog  time.Time
}

// NewStore returns a Store retaining up to MaxJourneys journeys.
func NewStore() *Store {
	return NewStoreWithCap(MaxJourneys)
}

// NewStoreWithCap returns a Store with an explicit journey capacity. A
// non-positive capacity falls back to MaxJourneys so a Store is never
// zero-capacity.
func NewStoreWithCap(capacity int) *Store {
	if capacity <= 0 {
		capacity = MaxJourneys
	}
	return &Store{
		journeys: make(map[string]*Journey, capacity),
		cap:      capacity,
	}
}

// EnablePersistence loads any journeys previously saved at path into the
// store and turns on atomic re-persistence (throttled by persistMinInterval).
// Call it once, before producers start recording.
//
// Unlike the mutation claim ledger, the timeline is derived telemetry, not
// ownership state — so a corrupt file must not refuse startup. An unparseable
// file is moved aside to path+".corrupt" for inspection and the store starts
// empty. An unreadable-but-existing file is reported via the returned error
// (persistence stays enabled; the next save overwrites).
func (s *Store) EnablePersistence(path string, logger *slog.Logger) error {
	if s == nil || path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
	s.logger = logger

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading lifecycle timeline %s: %w", path, err)
	}
	var persisted persistedTimeline
	if err := json.Unmarshal(data, &persisted); err != nil {
		// Telemetry, not authority: keep the bytes for inspection, start empty.
		_ = os.Rename(path, path+".corrupt")
		if logger != nil {
			logger.Warn("lifecycle timeline file corrupt — moved aside, starting empty",
				"path", path, "error", err)
		}
		return nil
	}
	for i := range persisted.Journeys {
		j := persisted.Journeys[i]
		if j.Ref == "" || len(j.Stages) == 0 {
			continue
		}
		cp := j.clone()
		// Re-derive rather than trust the file: Current is a pure function of
		// the stages, and re-deriving keeps old files honest across upgrades.
		cp.Current = deriveCurrent(&cp)
		s.journeys[j.Ref] = &cp
	}
	s.enforceCapLocked()
	return nil
}

// Record folds an event into its journey, keyed by IssueRef+Kind: a first
// recording creates the stage; a repeat refreshes LastAt/Count/attrs on the
// existing one, so re-enumeration never floods the store. If At is unset
// (<= 0) it is stamped with the current time. Events with an empty IssueRef
// or Kind carry no journey identity and are dropped. Safe for concurrent use.
func (s *Store) Record(e Event) {
	if s == nil || e.IssueRef == "" || e.Kind == "" {
		return
	}
	at := e.At
	if at <= 0 {
		at = time.Now().UnixMilli()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	j := s.journeys[e.IssueRef]
	if j == nil {
		s.enforceCapForOneMoreLocked()
		j = &Journey{
			Ref:     e.IssueRef,
			FirstAt: at,
			LastAt:  at,
			Stages:  make(map[Kind]*Stage, 4),
		}
		s.journeys[e.IssueRef] = j
	}

	st := j.Stages[e.Kind]
	if st == nil {
		st = &Stage{FirstAt: at, LastAt: at}
		j.Stages[e.Kind] = st
	}
	st.Count++
	if at > st.LastAt {
		st.LastAt = at
	}
	if at < st.FirstAt {
		st.FirstAt = at
	}
	if e.Agent != "" {
		st.Agent = e.Agent
		j.Agent = e.Agent
	}
	if len(e.Attrs) > 0 {
		if st.Attrs == nil {
			st.Attrs = make(map[string]string, len(e.Attrs))
		}
		for k, v := range e.Attrs {
			st.Attrs[k] = v
		}
	}
	if at < j.FirstAt {
		j.FirstAt = at
	}
	if at > j.LastAt {
		j.LastAt = at
	}
	j.Current = deriveCurrent(j)

	s.dirty = true
	s.persistMaybeLocked(e.Kind)
}

// deriveCurrent computes a journey's current stage: merged is sticky-terminal;
// blocked holds only while no progress stage fired at-or-after it; otherwise
// the furthest progress stage reached. A journey with only unknown kinds falls
// back to its most recently touched stage.
func deriveCurrent(j *Journey) Kind {
	if j.Stages[KindMerged] != nil {
		return KindMerged
	}
	latestProgress := int64(-1)
	for _, k := range progressKinds {
		if st := j.Stages[k]; st != nil && st.LastAt > latestProgress {
			latestProgress = st.LastAt
		}
	}
	if b := j.Stages[KindBlocked]; b != nil && b.LastAt >= latestProgress {
		return KindBlocked
	}
	for _, k := range progressKinds {
		if j.Stages[k] != nil {
			return k
		}
	}
	// Only unknown kinds (or only a cleared block): most recent stage wins.
	var current Kind
	var latest int64 = -1
	for k, st := range j.Stages {
		if st.LastAt > latest || (st.LastAt == latest && k < current) {
			current, latest = k, st.LastAt
		}
	}
	return current
}

// enforceCapForOneMoreLocked makes room for one new journey. Caller holds mu.
func (s *Store) enforceCapForOneMoreLocked() {
	for len(s.journeys) >= s.cap {
		s.evictOldestLocked()
	}
}

// enforceCapLocked trims to capacity after a bulk load. Caller holds mu.
func (s *Store) enforceCapLocked() {
	for len(s.journeys) > s.cap {
		s.evictOldestLocked()
	}
}

// evictOldestLocked removes the least-recently-touched journey. Caller holds
// mu and guarantees the map is non-empty.
func (s *Store) evictOldestLocked() {
	var oldestRef string
	var oldestAt int64
	first := true
	for ref, j := range s.journeys {
		if first || j.LastAt < oldestAt || (j.LastAt == oldestAt && ref < oldestRef) {
			oldestRef, oldestAt, first = ref, j.LastAt, false
		}
	}
	delete(s.journeys, oldestRef)
}

// Journeys returns up to n journeys, most recently touched first (ties broken
// by ref for determinism). A non-positive n returns all retained journeys.
// Always returns a non-nil slice of deep copies.
func (s *Store) Journeys(n int) []Journey {
	if s == nil {
		return []Journey{}
	}
	s.mu.RLock()
	out := make([]Journey, 0, len(s.journeys))
	for _, j := range s.journeys {
		out = append(out, j.clone())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, k int) bool {
		if out[i].LastAt != out[k].LastAt {
			return out[i].LastAt > out[k].LastAt
		}
		return out[i].Ref < out[k].Ref
	})
	if n > 0 && n < len(out) {
		out = out[:n]
	}
	return out
}

// Journey returns a deep copy of the journey for ref, if one is retained.
func (s *Store) Journey(ref string) (Journey, bool) {
	if s == nil || ref == "" {
		return Journey{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	j := s.journeys[ref]
	if j == nil {
		return Journey{}, false
	}
	return j.clone(), true
}

// synthesizeEvents renders a journey's stages back into per-stage Events
// (newest first is the caller's job). Count is carried in attrs so consumers
// of the event view can see collapse cardinality.
func synthesizeEvents(j Journey) []Event {
	events := make([]Event, 0, len(j.Stages))
	for k, st := range j.Stages {
		attrs := st.Attrs
		if st.Count > 1 {
			attrs = make(map[string]string, len(st.Attrs)+1)
			for ak, av := range st.Attrs {
				attrs[ak] = av
			}
			attrs["count"] = fmt.Sprintf("%d", st.Count)
		}
		events = append(events, Event{
			ID:       j.Ref + ":" + string(k),
			IssueRef: j.Ref,
			Kind:     k,
			Agent:    st.Agent,
			At:       st.LastAt,
			Attrs:    attrs,
		})
	}
	return events
}

// Recent returns up to n most-recent stage events, newest first, synthesized
// from the retained journeys (one event per reached stage). A non-positive n
// returns all. Always returns a non-nil slice.
func (s *Store) Recent(n int) []Event {
	if s == nil {
		return []Event{}
	}
	all := make([]Event, 0)
	for _, j := range s.Journeys(0) {
		all = append(all, synthesizeEvents(j)...)
	}
	sort.Slice(all, func(i, k int) bool {
		if all[i].At != all[k].At {
			return all[i].At > all[k].At
		}
		return all[i].ID < all[k].ID
	})
	if n > 0 && n < len(all) {
		all = all[:n]
	}
	return all
}

// ByIssue returns the stage events for the given ref, newest first. An empty
// or unknown ref yields an empty (non-nil) slice. Note: repeats of a stage are
// collapsed (Count rides in attrs); consumers needing true cardinality should
// use Journey and read Stage.Count (see pkg/retro).
func (s *Store) ByIssue(ref string) []Event {
	j, ok := s.Journey(ref)
	if !ok {
		return []Event{}
	}
	events := synthesizeEvents(j)
	sort.Slice(events, func(i, k int) bool {
		if events[i].At != events[k].At {
			return events[i].At > events[k].At
		}
		return events[i].ID < events[k].ID
	})
	return events
}

// FleetHealth is a derived roll-up of lifecycle activity over a rolling
// window, honest about how much history actually backs it (#5656).
type FleetHealth struct {
	// WindowMs is the requested look-back window, in ms.
	WindowMs int64 `json:"windowMs"`
	// CoveredMs is how much of that window the retained journeys actually
	// span (<= WindowMs). A UI must label the counts with this, not the
	// requested window, or "0 merged" reads as "nothing ships" when the truth
	// is "no history yet".
	CoveredMs int64 `json:"coveredMs"`
	// InFlight is the number of journeys active in the window whose current
	// stage is non-terminal.
	InFlight int `json:"inFlight"`
	// Merged / Blocked count journeys active in the window whose current
	// stage is merged / blocked.
	Merged  int `json:"merged"`
	Blocked int `json:"blocked"`
	// Events is the number of journeys with any activity inside the window.
	Events int `json:"events"`
}

// DeriveFleetHealth rolls up the given journeys over the window (measured
// back from now). A non-positive window uses DefaultFleetWindow. Exported so
// callers that pre-filter journeys (the dashboard's ACMM gate) derive counts
// with exactly the Store's semantics.
func DeriveFleetHealth(journeys []Journey, window time.Duration) FleetHealth {
	if window <= 0 {
		window = DefaultFleetWindow
	}
	fh := FleetHealth{WindowMs: window.Milliseconds()}
	now := time.Now().UnixMilli()
	cutoff := now - fh.WindowMs

	oldest := int64(0)
	for i := range journeys {
		j := &journeys[i]
		if oldest == 0 || j.FirstAt < oldest {
			oldest = j.FirstAt
		}
		if j.LastAt < cutoff {
			continue
		}
		fh.Events++
		switch j.Current {
		case KindMerged:
			fh.Merged++
		case KindBlocked:
			fh.Blocked++
		default:
			fh.InFlight++
		}
	}
	if oldest > 0 {
		span := now - oldest
		if span < 0 {
			span = 0
		}
		if span > fh.WindowMs {
			span = fh.WindowMs
		}
		fh.CoveredMs = span
	}
	return fh
}

// FleetHealth derives the roll-up over this store's retained journeys. Safe
// for concurrent use.
func (s *Store) FleetHealth(window time.Duration) FleetHealth {
	if s == nil {
		return DeriveFleetHealth(nil, window)
	}
	return DeriveFleetHealth(s.Journeys(0), window)
}

// TimelineDTO is the JSON contract the dashboard consumes: the most recently
// active journeys plus the derived fleet health. Journeys is always an array
// in the serialized form, never null.
type TimelineDTO struct {
	Journeys []Journey   `json:"journeys"`
	Fleet    FleetHealth `json:"fleet"`
}

// Snapshot builds a TimelineDTO with up to n journeys (most recently active
// first) and fleet health over the given window. Non-positive n / window fall
// back to "all journeys" / DefaultFleetWindow. Never returns nil slices.
func (s *Store) Snapshot(n int, window time.Duration) TimelineDTO {
	journeys := s.Journeys(n)
	if journeys == nil {
		journeys = []Journey{}
	}
	attachJourneyDetails(journeys, time.Now())
	return TimelineDTO{
		Journeys: journeys,
		Fleet:    s.FleetHealth(window),
	}
}

func attachJourneyDetails(journeys []Journey, now time.Time) {
	nowMs := now.UnixMilli()
	for i := range journeys {
		journeys[i].Details = buildJourneyDetails(journeys[i], nowMs)
	}
}

func buildJourneyDetails(j Journey, nowMs int64) *JourneyDetails {
	allAttrs := map[string]string{}
	for _, st := range j.Stages {
		if st == nil {
			continue
		}
		for k, v := range st.Attrs {
			if strings.TrimSpace(v) != "" {
				allAttrs[k] = v
			}
		}
	}
	d := &JourneyDetails{
		Stages: buildStageDetails(j.Stages),
		Agent:  j.Agent,
	}
	if len(allAttrs) > 0 {
		d.Attrs = allAttrs
	}
	d.Classification = buildClassificationInfo(j.Stages, allAttrs)
	d.Links = buildJourneyLinks(j.Ref, allAttrs)
	d.BlockedReason = blockedReason(j.Stages, allAttrs)
	d.KickIDs = attrValues(allAttrs, "kick_id", "kick", "task_id", "event.id", "event_id")
	d.RunLogIDs = attrValues(allAttrs, "run_log_id", "run_id", "run_key", "run", "gen", "path")
	if st := j.Stages[j.Current]; st != nil && nowMs > st.LastAt {
		d.TimeInCurrentStage = nowMs - st.LastAt
	}
	return d
}

func buildStageDetails(stages map[Kind]*Stage) []StageDetail {
	out := make([]StageDetail, 0, len(stages))
	for kind, st := range stages {
		if st == nil {
			continue
		}
		detail := StageDetail{
			Kind:    kind,
			Label:   stageLabel(kind),
			FirstAt: st.FirstAt,
			LastAt:  st.LastAt,
			Count:   st.Count,
			Agent:   st.Agent,
		}
		if len(st.Attrs) > 0 {
			detail.Attrs = make(map[string]string, len(st.Attrs))
			for k, v := range st.Attrs {
				detail.Attrs[k] = v
			}
		}
		out = append(out, detail)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].FirstAt != out[k].FirstAt {
			return out[i].FirstAt < out[k].FirstAt
		}
		return kindOrder(out[i].Kind) < kindOrder(out[k].Kind)
	})
	for i := 1; i < len(out); i++ {
		if out[i].FirstAt > out[i-1].LastAt {
			out[i].DurationSincePreviousMs = out[i].FirstAt - out[i-1].LastAt
		}
	}
	if out == nil {
		return []StageDetail{}
	}
	return out
}

func blockedReason(stages map[Kind]*Stage, attrs map[string]string) string {
	if st := stages[KindBlocked]; st != nil && len(st.Attrs) > 0 {
		if reason := firstAttr(st.Attrs, "blocked_reason", "block_reason", "reason", "waiting_reason", "waiting_on", "severity"); reason != "" {
			return reason
		}
	}
	return firstAttr(attrs, "blocked_reason", "block_reason", "waiting_reason", "waiting_on", "severity")
}

func buildClassificationInfo(stages map[Kind]*Stage, attrs map[string]string) *ClassificationInfo {
	classAttrs := attrs
	if st := stages[KindClassified]; st != nil && len(st.Attrs) > 0 {
		classAttrs = st.Attrs
	}
	info := &ClassificationInfo{
		Band:   firstAttr(classAttrs, "band", "classification_band", "intent_tier"),
		Lane:   firstAttr(classAttrs, "lane"),
		Tier:   firstAttr(classAttrs, "tier", "complexity_tier"),
		Model:  firstAttr(classAttrs, "model", "model_rec"),
		Reason: firstAttr(classAttrs, "reason", "rationale", "classification_reason", "triage_rationale"),
	}
	if info.Band == "" && info.Tier != "" {
		info.Band = info.Tier
	}
	if info.Band == "" && info.Lane == "" && info.Tier == "" && info.Model == "" && info.Reason == "" {
		return nil
	}
	return info
}

func buildJourneyLinks(ref string, attrs map[string]string) []JourneyLink {
	repo, issue := splitRef(ref)
	links := []JourneyLink{}
	if repo != "" && issue != "" {
		links = append(links, JourneyLink{Kind: "issue", Label: "Issue #" + issue, URL: "https://github.com/" + repo + "/issues/" + issue, Number: issue})
	}
	if url := firstSafeExternalURL(attrs, "issue_url", "issue.html_url"); url != "" {
		links = upsertLink(links, JourneyLink{Kind: "issue", Label: "Issue", URL: url})
	}
	if pr := normalizeNumber(firstAttr(attrs, "pr", "pr_number", "pull", "pull_request", "pull_number")); repo != "" && pr != "" {
		links = append(links, JourneyLink{Kind: "pr", Label: "PR #" + pr, URL: "https://github.com/" + repo + "/pull/" + pr, Number: pr})
	}
	if url := firstSafeExternalURL(attrs, "pr_url", "pull_url", "pull_request_url", "html_url"); url != "" {
		links = upsertLink(links, JourneyLink{Kind: "pr", Label: "PR", URL: url})
	}
	return links
}

func upsertLink(links []JourneyLink, link JourneyLink) []JourneyLink {
	if strings.TrimSpace(link.URL) == "" {
		return links
	}
	for i := range links {
		if links[i].Kind == link.Kind {
			links[i].URL = link.URL
			if link.Number != "" {
				links[i].Number = link.Number
			}
			return links
		}
	}
	return append(links, link)
}

func splitRef(ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	idx := strings.LastIndex(ref, "#")
	if idx < 0 {
		return "", ""
	}
	repo := strings.TrimSpace(ref[:idx])
	num := normalizeNumber(ref[idx+1:])
	if repo == "" || num == "" || !strings.Contains(repo, "/") {
		return "", ""
	}
	return repo, num
}

func firstSafeExternalURL(attrs map[string]string, keys ...string) string {
	for _, key := range keys {
		if url := safeExternalURL(attrs[key]); url != "" {
			return url
		}
	}
	return ""
}

func safeExternalURL(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") {
		return v
	}
	return ""
}

func normalizeNumber(v string) string {
	v = strings.TrimSpace(strings.TrimPrefix(v, "#"))
	if v == "" {
		return ""
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return strconv.Itoa(n)
	}
	return ""
}

func firstAttr(attrs map[string]string, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(attrs[key]); v != "" {
			return v
		}
	}
	return ""
}

func attrValues(attrs map[string]string, keys ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, key := range keys {
		if v := strings.TrimSpace(attrs[key]); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func stageLabel(kind Kind) string {
	switch kind {
	case KindEnumerated:
		return "Enumerated"
	case KindClassified:
		return "Classified"
	case KindKicked:
		return "Kicked"
	case KindPROpened:
		return "PR opened"
	case KindMerged:
		return "Merged"
	case KindBlocked:
		return "Blocked"
	case KindStageCompleted:
		return "Stage completed"
	case KindStageReceipt:
		return "Stage receipt"
	case KindStageApproval:
		return "Stage approval"
	default:
		label := strings.ReplaceAll(string(kind), "_", " ")
		if label == "" {
			return "Unknown"
		}
		return strings.ToUpper(label[:1]) + label[1:]
	}
}

func kindOrder(kind Kind) int {
	for i, known := range KnownKinds() {
		if kind == known {
			return i
		}
	}
	switch kind {
	case KindStageCompleted:
		return len(KnownKinds())
	case KindStageReceipt:
		return len(KnownKinds()) + 1
	case KindStageApproval:
		return len(KnownKinds()) + 2
	default:
		return len(KnownKinds()) + 10
	}
}

// persistMaybeLocked writes the journeys file if persistence is enabled and
// either the recorded stage is precious
// (pr_opened/merged/blocked/stage_completed/stage_approval — outcomes,
// decisions, and run transitions the panel exists to keep) or the throttle
// interval has elapsed. Caller holds mu.
// Errors never propagate to producers; they are logged, throttled.
func (s *Store) persistMaybeLocked(kind Kind) {
	if s.path == "" || !s.dirty {
		return
	}
	force := kind == KindPROpened || kind == KindMerged || kind == KindBlocked || kind == KindStageCompleted || kind == KindStageApproval
	if !force && time.Since(s.lastPersist) < persistMinInterval {
		return
	}
	if err := s.persistLocked(); err != nil {
		if time.Since(s.lastErrLog) >= persistErrLogInterval {
			s.lastErrLog = time.Now()
			if s.logger != nil {
				s.logger.Warn("lifecycle timeline persist failed — journeys will reset on restart",
					"path", s.path, "error", err)
			}
		}
		return
	}
	s.dirty = false
	s.lastPersist = time.Now()
}

// persistLocked rewrites the journeys file with the repo's atomic-persist
// idiom (unique temp file + fsync + rename, per pkg/convergence/mutation
// post-#5625): a crash mid-write can never leave a truncated file, and a
// non-cooperating writer on the same path can never corrupt a commit. Caller
// holds mu.
func (s *Store) persistLocked() error {
	all := make([]Journey, 0, len(s.journeys))
	for _, j := range s.journeys {
		all = append(all, *j)
	}
	sort.Slice(all, func(i, k int) bool { return all[i].Ref < all[k].Ref })
	data, err := json.Marshal(persistedTimeline{Version: timelineFormatVersion, Journeys: all})
	if err != nil {
		return fmt.Errorf("marshaling lifecycle timeline: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating tmp lifecycle timeline: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(timelineFileMode); err != nil {
		return fmt.Errorf("protecting tmp lifecycle timeline: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing tmp lifecycle timeline: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing tmp lifecycle timeline: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing tmp lifecycle timeline: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("committing lifecycle timeline: %w", err)
	}
	keep = true
	return nil
}

// spanKindMap maps known span names to lifecycle Kinds for FromSpan. Keeping it
// here (rather than in tracing) preserves the decoupling: the tracing/governor
// side names spans however it likes; the timeline owns the translation.
var spanKindMap = map[string]Kind{
	"governor.enumerate":  KindEnumerated,
	"governor.classify":   KindClassified,
	"governor.kick":       KindKicked,
	"agent.kick":          KindKicked,
	"agent.kicked":        KindKicked,
	"pr.opened":           KindPROpened,
	"pr.open":             KindPROpened,
	"pr.merged":           KindMerged,
	"pr.merge":            KindMerged,
	"issue.blocked":       KindBlocked,
	"agent.blocked":       KindBlocked,
	"lifecycle.enumerate": KindEnumerated,
	"lifecycle.classify":  KindClassified,
	"lifecycle.kick":      KindKicked,
	"lifecycle.pr_opened": KindPROpened,
	"lifecycle.merged":    KindMerged,
	"lifecycle.blocked":   KindBlocked,
}

// Attribute keys FromSpan understands. Attrs unrecognized here are still copied
// verbatim into Event.Attrs.
const (
	attrIssueRef = "issue.ref"
	attrAgent    = "agent"
	attrEventID  = "event.id"
)

// FromSpan translates a span name + attribute map into a lifecycle Event. It
// returns (Event, false) when the span name does not correspond to a lifecycle
// stage, so a span-processor adapter can cheaply skip non-lifecycle spans.
//
// Recognized attributes (issue.ref, agent, event.id) populate the typed Event
// fields; all attributes are also preserved in Event.Attrs. A nil attrs map is
// tolerated.
func FromSpan(name string, attrs map[string]string) (Event, bool) {
	kind, ok := spanKindMap[name]
	if !ok {
		return Event{}, false
	}
	e := Event{Kind: kind}
	if len(attrs) > 0 {
		e.ID = attrs[attrEventID]
		e.IssueRef = attrs[attrIssueRef]
		e.Agent = attrs[attrAgent]
		// Copy the full attr set so nothing is lost.
		cp := make(map[string]string, len(attrs))
		for k, v := range attrs {
			cp[k] = v
		}
		e.Attrs = cp
	}
	return e, true
}

// KnownKinds returns the closed set of lifecycle Kinds, ordered along the
// issue→PR journey. Handy for UIs building a fixed lifecycle axis.
func KnownKinds() []Kind {
	return []Kind{
		KindEnumerated,
		KindClassified,
		KindKicked,
		KindPROpened,
		KindMerged,
		KindBlocked,
	}
}
