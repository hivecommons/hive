package sessiontaint

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Audit action names the observer reports through Options.Audit. Snake_case to
// match the audit log's existing vocabulary.
const (
	// AuditLabelSet is written once per (session, label, origin).
	AuditLabelSet = "session_taint_label"
	// AuditWouldAct is written once per distinct would-act decision per
	// session (rule, sink kind, destination), not once per request.
	AuditWouldAct = "session_taint_would_act"
)

const (
	// maxFieldLen bounds every identifier the observer stores or logs.
	maxFieldLen = 128
	// invalidField replaces an identifier that fails sanitisation, so
	// free-form text can never be smuggled into the store, the log or the
	// audit trail through an origin, source or agent name.
	invalidField = "invalid"
	// maxCategoryLen bounds a secret label's origin, which must be a
	// category slug (letters and hyphens), so a credential value, which
	// carries digits, upper case or underscores and is longer, is refused as
	// an origin and stored as invalidField instead.
	maxCategoryLen = 40
	// unspecifiedOrigin is the origin recorded when a caller gives none.
	unspecifiedOrigin = "unspecified"
	// modeObserve is the only mode this phase has; it is reported in Stats,
	// the log and the audit detail so a reader cannot mistake it for
	// enforcement.
	modeObserve = "observe"
)

// Options configures an Observer. The zero value of every bound means its
// default.
type Options struct {
	// Enabled gates the whole feature. New returns nil when it is false, and
	// every Observer method is a no-op on nil, so a hive that sets nothing
	// records nothing and runs byte-for-byte as before.
	Enabled bool
	// MaxSessions, MaxLabelsPerSession and MaxDecisions override the default
	// bounds when positive.
	MaxSessions         int
	MaxLabelsPerSession int
	MaxDecisions        int
	// Logger receives one line per new label and per new would-act decision.
	// Nil means no log lines; counters and the store still work.
	Logger *slog.Logger
	// Audit, when set, receives the AuditLabelSet and AuditWouldAct entries.
	// The detail carries ids and labels only. It must not block.
	Audit func(agent, action, detail string)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Decision is one recorded would-act result. It names the rule, the label and
// ids; it carries no content.
type Decision struct {
	At       time.Time
	Agent    string
	Rule     string
	Verdict  Verdict
	Sink     SinkKind
	DestRepo string
	Label    Label
	Origin   string
}

// Stats is a point-in-time copy of the observer's counters and recent
// decisions, for tests and for the read-only API a later phase adds.
type Stats struct {
	Mode            string
	Sessions        int
	Labels          int
	SessionsEvicted uint64
	LabelsRecorded  uint64
	LabelsDropped   uint64
	SinksObserved   uint64
	SinksClean      uint64
	Panics          uint64
	// WouldAct counts would-act findings per rule id, every request counted.
	WouldAct map[string]uint64
	// Recent is the bounded ring of the latest would-act decisions, oldest
	// first.
	Recent []Decision
}

// Observer records labels and would-be verdicts. It is observe-only: no
// method returns a verdict or an error, so nothing the host does can depend
// on it, and it holds no reference to any request.
type Observer struct {
	opts  Options
	store *store

	mu             sync.Mutex
	labelsRecorded uint64
	sinksObserved  uint64
	sinksClean     uint64
	panics         uint64
	wouldAct       map[string]uint64
	ring           []Decision
	ringStart      int
}

// New builds an Observer, or nil when opts.Enabled is false.
func New(opts Options) *Observer {
	if !opts.Enabled {
		return nil
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = DefaultMaxSessions
	}
	if opts.MaxLabelsPerSession <= 0 {
		opts.MaxLabelsPerSession = DefaultMaxLabelsPerSession
	}
	if opts.MaxDecisions <= 0 {
		opts.MaxDecisions = DefaultMaxDecisions
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Observer{
		opts:     opts,
		store:    newStore(opts.MaxSessions, opts.MaxLabelsPerSession),
		wouldAct: make(map[string]uint64),
	}
}

// RecordLabel notes that the hive observed something in the agent's session.
// An empty agent is an unattributed caller and is not recorded (design:
// failure table). Safe on a nil Observer.
func (o *Observer) RecordLabel(agent string, label Label, origin string, source Source) {
	if o == nil {
		return
	}
	defer o.recoverQuiet()
	agent = cleanField(agent)
	if agent == "" || agent == invalidField {
		return
	}
	if !knownLabel(label) {
		return
	}
	origin = originOrDefault(cleanField(origin))
	if !validOrigin(label, origin) {
		origin = invalidField
	}
	if !knownSource(source) {
		source = Source(invalidField)
	}
	rec := LabelRecord{
		Label:     label,
		Origin:    origin,
		Source:    source,
		FirstSeen: o.opts.Now(),
	}
	added, dropped := o.store.add(agent, rec)
	switch {
	case dropped:
		// Counted in Stats.LabelsDropped; not logged per attempt, so a
		// session at its bound cannot flood the log.
	case added:
		o.mu.Lock()
		o.labelsRecorded++
		o.mu.Unlock()
		o.logInfo("session taint label set (observe only)",
			"agent", agent, "label", string(rec.Label), "origin", rec.Origin, "source", string(rec.Source))
		o.audit(agent, AuditLabelSet, fmt.Sprintf("label=%s, origin=%s, source=%s, mode=%s", rec.Label, rec.Origin, rec.Source, modeObserve))
	}
}

// ObserveSink evaluates one outbound action against the agent's labels and
// records any would-act findings. audienceOf resolves the destination's
// audience and is called only when the session holds at least one label, so a
// clean session costs a map lookup and no network call. A panic in audienceOf
// is absorbed and counted. Nothing is returned: the action proceeds exactly as
// it would have with no observer.
func (o *Observer) ObserveSink(agent string, kind SinkKind, repo string, audienceOf func(repo string) Audience) {
	if o == nil {
		return
	}
	defer o.recoverQuiet()
	agent = cleanField(agent)
	if agent == "" || agent == invalidField {
		return
	}
	repo = cleanField(repo)
	labels := o.store.labelsOf(agent)
	o.mu.Lock()
	o.sinksObserved++
	if len(labels) == 0 {
		o.sinksClean++
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()

	aud := AudiencePublic
	if audienceOf != nil {
		aud = o.lookupAudience(audienceOf, repo)
	}
	findings := Evaluate(labels, Sink{Kind: kind, Repo: repo, Audience: aud})
	for _, f := range findings {
		o.recordFinding(agent, kind, repo, f)
	}
}

// lookupAudience runs the host's audience lookup, treating a panic as
// "public" (the conservative answer for a destination).
func (o *Observer) lookupAudience(audienceOf func(string) Audience, repo string) (aud Audience) {
	aud = AudiencePublic
	defer func() {
		if r := recover(); r != nil {
			o.mu.Lock()
			o.panics++
			o.mu.Unlock()
			aud = AudiencePublic
		}
	}()
	return audienceOf(repo)
}

func (o *Observer) recordFinding(agent string, kind SinkKind, repo string, f Finding) {
	d := Decision{
		At: o.opts.Now(), Agent: agent, Rule: f.Rule, Verdict: f.Verdict,
		Sink: kind, DestRepo: repo, Label: f.Label, Origin: f.Origin,
	}
	o.mu.Lock()
	o.wouldAct[f.Rule]++
	o.mu.Unlock()
	key := f.Rule + "|" + string(kind) + "|" + strings.ToLower(repo)
	if !o.store.firstDecision(agent, key) {
		return
	}
	o.mu.Lock()
	if len(o.ring) < o.opts.MaxDecisions {
		o.ring = append(o.ring, d)
	} else {
		o.ring[o.ringStart] = d
		o.ringStart = (o.ringStart + 1) % o.opts.MaxDecisions
	}
	o.mu.Unlock()
	o.logInfo("session taint would act (observe only, nothing blocked)",
		"agent", agent, "rule", d.Rule, "verdict", string(d.Verdict), "sink", string(kind),
		"dest", repo, "label", string(d.Label), "origin", d.Origin)
	o.audit(agent, AuditWouldAct, fmt.Sprintf("rule=%s, verdict=%s, sink=%s, dest=%s, label=%s, origin=%s, mode=%s",
		d.Rule, d.Verdict, kind, repo, d.Label, d.Origin, modeObserve))
}

// Stats returns a copy of the counters and the recent decisions. Safe on a nil
// Observer, which reports the zero Stats.
func (o *Observer) Stats() Stats {
	if o == nil {
		return Stats{}
	}
	c := o.store.counts()
	o.mu.Lock()
	defer o.mu.Unlock()
	st := Stats{
		Mode:            modeObserve,
		Sessions:        c.sessions,
		Labels:          c.labels,
		SessionsEvicted: c.evicted,
		LabelsRecorded:  o.labelsRecorded,
		LabelsDropped:   c.labelsDropped,
		SinksObserved:   o.sinksObserved,
		SinksClean:      o.sinksClean,
		Panics:          o.panics,
		WouldAct:        make(map[string]uint64, len(o.wouldAct)),
	}
	for k, v := range o.wouldAct {
		st.WouldAct[k] = v
	}
	st.Recent = append(st.Recent, o.ring[o.ringStart:]...)
	st.Recent = append(st.Recent, o.ring[:o.ringStart]...)
	return st
}

// Labels returns a copy of one agent's labels, nil when it has none. Safe on a
// nil Observer.
func (o *Observer) Labels(agent string) []LabelRecord {
	if o == nil {
		return nil
	}
	return o.store.labelsOf(cleanField(agent))
}

// recoverQuiet absorbs a panic so an observation can never break the relay or
// handler it is called from.
func (o *Observer) recoverQuiet() {
	if r := recover(); r != nil {
		o.mu.Lock()
		o.panics++
		o.mu.Unlock()
	}
}

func (o *Observer) logInfo(msg string, args ...any) {
	if o.opts.Logger != nil {
		o.opts.Logger.Info(msg, args...)
	}
}

func (o *Observer) audit(agent, action, detail string) {
	if o.opts.Audit != nil {
		o.opts.Audit(agent, action, detail)
	}
}

func knownLabel(l Label) bool {
	switch l {
	case LabelSecret, LabelPrivateRepo, LabelCredentialedResponse, LabelUnknown:
		return true
	}
	return false
}

// validOrigin reports whether origin has the shape its label allows, so a
// credential value or free text can never be kept as an origin:
//   - secret and unknown: a category or reason slug of lower-case letters and
//     hyphens ("github-token", "state-lost"), at most maxCategoryLen long. A
//     credential carries digits, upper case or underscores and is longer;
//   - private-repo: an owner/repo id;
//   - credentialed-response: a host name.
//
// The placeholder invalidField is always allowed, and is how a refused origin
// is stored.
func validOrigin(l Label, origin string) bool {
	if origin == invalidField || origin == unspecifiedOrigin {
		return true
	}
	switch l {
	case LabelSecret, LabelUnknown:
		return isCategorySlug(origin)
	case LabelPrivateRepo:
		return isRepoID(origin)
	case LabelCredentialedResponse:
		return isHost(origin)
	}
	return false
}

// isCategorySlug reports whether s is 1 to maxCategoryLen lower-case letters
// and hyphens.
func isCategorySlug(s string) bool {
	if s == "" || len(s) > maxCategoryLen {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return true
}

// isRepoID reports whether s is exactly owner/repo, both parts non-empty and
// limited to letters, digits, '.', '_' and '-'.
func isRepoID(s string) bool {
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return false
	}
	return onlyIdentRunes(owner, "._-") && onlyIdentRunes(repo, "._-")
}

// isHost reports whether s is a host name, with an optional :port.
func isHost(s string) bool {
	return s != "" && onlyIdentRunes(s, ".-:")
}

func onlyIdentRunes(s, extra string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(extra, r):
		default:
			return false
		}
	}
	return true
}

func knownSource(s Source) bool {
	switch s {
	case SourceCanaryHit:
		return true
	}
	return false
}

func originOrDefault(s string) string {
	if s == "" {
		return unspecifiedOrigin
	}
	return s
}

// cleanField reduces an identifier to a short, log-safe form: trimmed, at most
// maxFieldLen bytes, and only letters, digits and . _ - : / @ +. Anything else
// (whitespace, quotes, newlines, free text) becomes invalidField. An empty
// input stays empty.
func cleanField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > maxFieldLen {
		return invalidField
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._-:/@+", r):
		default:
			return invalidField
		}
	}
	return s
}
