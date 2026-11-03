package github

import (
	"context"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Shadow-mode would-notify instrumentation (github.notifications.shadow,
// hivecommons/hive#16264). Every notification-capable write the client
// makes — a new issue/PR comment, a review submission, a review request,
// and the @-mentions a comment or review body carries — is attributed to
// its producer (the inventoried function that asked for it, see
// notificationWriteSites), classified, and given a would-notify decision.
// Nothing is suppressed or rerouted: the decision is recorded, never acted
// on, and with the flag off the recording path returns before doing any
// work. Comment bodies, inbox contents and recipient lists are never kept;
// a record carries only the logins a write mentions or requests.

// NotificationClass is why a notification-capable write happened.
type NotificationClass string

const (
	// NotificationClassStatus is bookkeeping a person can read on demand:
	// stage notes, fleet reports, close/unblock notes, operator decision
	// records. Repeats of the same condition are would-suppress candidates.
	NotificationClassStatus NotificationClass = "status"
	// NotificationClassAttention asks a person to look or act (parked,
	// escalated, duplicate, clarification needed). Repeats of the same
	// condition are would-suppress candidates.
	NotificationClassAttention NotificationClass = "attention"
	// NotificationClassRequiredReview is a review submission or review
	// request a merge depends on. Never would-suppress.
	NotificationClassRequiredReview NotificationClass = "required_review"
	// NotificationClassProtocolCommand is a machine-consumed comment
	// (claim/release markers, /lgtm, /approve, /ok-to-test). Never
	// would-suppress: a reader acts on every instance.
	NotificationClassProtocolCommand NotificationClass = "protocol_command"
	// NotificationClassSecurityEvent is a trust or integrity signal
	// (reporter trust, self-authorization, hold-guard drift). Never
	// would-suppress.
	NotificationClassSecurityEvent NotificationClass = "security_event"
	// NotificationClassUnclassified marks a write whose producer is not in
	// the inventory. The inventory guard test keeps this at zero for call
	// sites under src/pkg and src/cmd.
	NotificationClassUnclassified NotificationClass = "unclassified"
)

// Suppressible reports whether a repeat of the same condition in this class
// may be marked would-suppress. Protocol commands, required reviews and
// security events are always delivered.
func (c NotificationClass) Suppressible() bool {
	return c == NotificationClassStatus || c == NotificationClassAttention
}

// Shadow counter names, exported as the metric label/series suffixes.
const (
	NotificationMetricCommentsCreated      = "comments_created"
	NotificationMetricReviewsSubmitted     = "reviews_submitted"
	NotificationMetricReviewRequests       = "review_requests"
	NotificationMetricMentions             = "mentions"
	NotificationMetricSuppressedRepeats    = "suppressed_repeats"
	NotificationMetricWouldSuppressRepeats = "would_suppress_repeats"
	NotificationMetricDeliveryFailures     = "delivery_failures"
)

// Would-notify reasons carried by a NotificationShadowRecord.
const (
	NotificationReasonNewCondition    = "new_condition"
	NotificationReasonRepeatCondition = "repeat_condition"
	NotificationReasonExemptRepeat    = "exempt_class_repeat"
	NotificationReasonDeliveryFailed  = "delivery_failed"
)

const (
	// notificationShadowWindow is how long a condition key counts as
	// already notified, and how long an item counts as active.
	notificationShadowWindow = 24 * time.Hour
	// notificationShadowMaxKeys bounds the condition and active-item maps.
	notificationShadowMaxKeys = 20000
	// notificationShadowLogSize bounds the local would-notify log.
	notificationShadowLogSize = 512
	// notificationShadowMaxLogins bounds the logins kept per record.
	notificationShadowMaxLogins = 10
)

type notificationWriteKind string

const (
	notificationWriteComment       notificationWriteKind = "comment"
	notificationWriteReview        notificationWriteKind = "review"
	notificationWriteReviewRequest notificationWriteKind = "review_request"
)

// NotificationShadowRecord is one would-notify decision. It never carries a
// comment body; Logins holds only the logins the write mentions or requests.
type NotificationShadowRecord struct {
	At       time.Time         `json:"at"`
	Repo     string            `json:"repo"`
	Item     int               `json:"item"`
	Kind     string            `json:"kind"`
	Producer string            `json:"producer"`
	Class    NotificationClass `json:"class"`
	// ConditionKey is the provisional shadow key (repo, item, class,
	// writer-supplied subject); the producer name stands in when the
	// writer supplied no subject.
	ConditionKey string   `json:"condition_key"`
	WouldNotify  bool     `json:"would_notify"`
	Reason       string   `json:"reason"`
	Logins       []string `json:"logins,omitempty"`
}

// NotificationShadowCount is one shadow counter value.
type NotificationShadowCount struct {
	Metric   string
	Producer string
	Class    NotificationClass
	Value    int64
}

type notificationShadowCounterKey struct {
	metric   string
	producer string
	class    NotificationClass
}

// notificationShadow holds the shadow counters, the condition and
// active-item memories, and the bounded would-notify log.
type notificationShadow struct {
	mu         sync.Mutex
	counters   map[notificationShadowCounterKey]int64
	conditions map[string]time.Time
	items      map[string]time.Time
	log        []NotificationShadowRecord
	next       int
}

type notificationSubjectKey struct{}

// WithNotificationSubject attaches the writer-supplied subject of the
// provisional condition key to ctx. A write without one uses its producer
// name as the subject.
func WithNotificationSubject(ctx context.Context, subject string) context.Context {
	if ctx == nil || strings.TrimSpace(subject) == "" {
		return ctx
	}
	return context.WithValue(ctx, notificationSubjectKey{}, subject)
}

func notificationSubjectFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(notificationSubjectKey{}).(string)
	return s
}

// SetNotificationsShadow wires github.notifications.shadow. Read live so a
// config reload applies to the next write. nil (nothing wired) is off.
func (c *Client) SetNotificationsShadow(fn func() bool) {
	if c == nil {
		return
	}
	c.shadowNotificationsFn = fn
}

func (c *Client) notificationsShadow() bool {
	return c != nil && c.shadowNotificationsFn != nil && c.shadowNotificationsFn()
}

// notificationWrite describes one notification-capable write at the
// boundary. body is scanned for @-mentions and then dropped.
type notificationWrite struct {
	kind      notificationWriteKind
	repo      string
	item      int
	body      string
	requested []string
	class     NotificationClass
	err       error
}

// recordNotificationWrite records the would-notify decision for one write.
// A no-op unless github.notifications.shadow is on.
func (c *Client) recordNotificationWrite(ctx context.Context, w notificationWrite) {
	if !c.notificationsShadow() {
		return
	}
	producer, site := attributeNotificationWrite()
	class := site.Class
	if w.class != "" {
		class = w.class
	}
	subject := notificationSubjectFrom(ctx)
	if subject == "" {
		subject = producer
	}
	logins := w.requested
	mentions := 0
	if w.kind != notificationWriteReviewRequest {
		logins = extractMentionLogins(w.body)
		mentions = len(logins)
	}
	if len(logins) > notificationShadowMaxLogins {
		logins = logins[:notificationShadowMaxLogins]
	}
	rec := NotificationShadowRecord{
		At:           notificationTallyNow(),
		Repo:         w.repo,
		Item:         w.item,
		Kind:         string(w.kind),
		Producer:     producer,
		Class:        class,
		ConditionKey: w.repo + "#" + strconv.Itoa(w.item) + "|" + string(class) + "|" + subject,
		Logins:       append([]string(nil), logins...),
	}
	c.notificationShadow.record(&rec, w, mentions)
	c.notifications.maybeLog(c.logger, c.notificationShadowLogAttrs()...)
}

func (s *notificationShadow) record(rec *NotificationShadowRecord, w notificationWrite, mentions int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counters == nil {
		s.counters = map[notificationShadowCounterKey]int64{}
		s.conditions = map[string]time.Time{}
		s.items = map[string]time.Time{}
	}
	now := rec.At
	bump := func(metric string, n int64) {
		if n > 0 {
			s.counters[notificationShadowCounterKey{metric, rec.Producer, rec.Class}] += n
		}
	}
	if w.err != nil {
		rec.Reason = NotificationReasonDeliveryFailed
		bump(NotificationMetricDeliveryFailures, 1)
		s.appendLog(*rec)
		return
	}
	switch w.kind {
	case notificationWriteComment:
		bump(NotificationMetricCommentsCreated, 1)
	case notificationWriteReview:
		bump(NotificationMetricReviewsSubmitted, 1)
	case notificationWriteReviewRequest:
		bump(NotificationMetricReviewRequests, 1)
	}
	bump(NotificationMetricMentions, int64(mentions))

	last, seen := s.conditions[rec.ConditionKey]
	repeat := seen && now.Sub(last) < notificationShadowWindow
	switch {
	case repeat && rec.Class.Suppressible():
		rec.WouldNotify = false
		rec.Reason = NotificationReasonRepeatCondition
		bump(NotificationMetricWouldSuppressRepeats, 1)
	case repeat:
		rec.WouldNotify = true
		rec.Reason = NotificationReasonExemptRepeat
	default:
		rec.WouldNotify = true
		rec.Reason = NotificationReasonNewCondition
	}
	s.conditions[rec.ConditionKey] = now
	s.items[rec.Repo+"#"+strconv.Itoa(rec.Item)] = now
	pruneNotificationShadowMap(s.conditions, now)
	pruneNotificationShadowMap(s.items, now)
	s.appendLog(*rec)
}

func (s *notificationShadow) appendLog(rec NotificationShadowRecord) {
	if len(s.log) < notificationShadowLogSize {
		s.log = append(s.log, rec)
		return
	}
	s.log[s.next] = rec
	s.next = (s.next + 1) % notificationShadowLogSize
}

// pruneNotificationShadowMap keeps m bounded: entries older than the window
// go first, then arbitrary ones until it fits.
func pruneNotificationShadowMap(m map[string]time.Time, now time.Time) {
	if len(m) <= notificationShadowMaxKeys {
		return
	}
	for k, t := range m {
		if now.Sub(t) >= notificationShadowWindow {
			delete(m, k)
		}
	}
	for k := range m {
		if len(m) <= notificationShadowMaxKeys {
			break
		}
		delete(m, k)
	}
}

// noteShadowSuppressed counts a note the hive actually chose not to post,
// labelled by its producer. A no-op unless shadow mode is on.
func (c *Client) noteShadowSuppressed() {
	if !c.notificationsShadow() {
		return
	}
	producer, site := attributeNotificationWrite()
	c.notificationShadow.mu.Lock()
	if c.notificationShadow.counters == nil {
		c.notificationShadow.counters = map[notificationShadowCounterKey]int64{}
		c.notificationShadow.conditions = map[string]time.Time{}
		c.notificationShadow.items = map[string]time.Time{}
	}
	c.notificationShadow.counters[notificationShadowCounterKey{NotificationMetricSuppressedRepeats, producer, site.Class}]++
	c.notificationShadow.mu.Unlock()
}

// NotificationShadowCounts returns the shadow counters, sorted by metric,
// producer and class. Empty while shadow mode has never been on.
func (c *Client) NotificationShadowCounts() []NotificationShadowCount {
	if c == nil {
		return nil
	}
	s := &c.notificationShadow
	s.mu.Lock()
	out := make([]NotificationShadowCount, 0, len(s.counters))
	for k, v := range s.counters {
		out = append(out, NotificationShadowCount{Metric: k.metric, Producer: k.producer, Class: k.class, Value: v})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Metric != b.Metric {
			return a.Metric < b.Metric
		}
		if a.Producer != b.Producer {
			return a.Producer < b.Producer
		}
		return a.Class < b.Class
	})
	return out
}

// NotificationShadowActiveItems is the number of distinct issues/PRs that
// received a notification-capable write in the last 24 hours — the
// denominator for per-active-item rates.
func (c *Client) NotificationShadowActiveItems() int {
	if c == nil {
		return 0
	}
	s := &c.notificationShadow
	now := notificationTallyNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.items {
		if now.Sub(t) < notificationShadowWindow {
			n++
		}
	}
	return n
}

// NotificationShadowEnabled reports whether github.notifications.shadow is on.
func (c *Client) NotificationShadowEnabled() bool {
	return c.notificationsShadow()
}

// NotificationShadowRecords returns the bounded would-notify log, oldest
// first.
func (c *Client) NotificationShadowRecords() []NotificationShadowRecord {
	if c == nil {
		return nil
	}
	s := &c.notificationShadow
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]NotificationShadowRecord, 0, len(s.log))
	out = append(out, s.log[s.next:]...)
	out = append(out, s.log[:s.next]...)
	return out
}

// notificationShadowLogAttrs are the shadow totals appended to the hourly
// "github notifications" line. Nil while shadow mode is off, so the line is
// unchanged.
func (c *Client) notificationShadowLogAttrs() []any {
	if !c.notificationsShadow() {
		return nil
	}
	totals := map[string]int64{}
	for _, cnt := range c.NotificationShadowCounts() {
		totals[cnt.Metric] += cnt.Value
	}
	return []any{
		"shadow_would_suppress", totals[NotificationMetricWouldSuppressRepeats],
		"shadow_reviews", totals[NotificationMetricReviewsSubmitted],
		"shadow_review_requests", totals[NotificationMetricReviewRequests],
		"shadow_mentions", totals[NotificationMetricMentions],
		"shadow_delivery_failures", totals[NotificationMetricDeliveryFailures],
		"shadow_active_items", c.NotificationShadowActiveItems(),
	}
}

// mentionLoginRe matches a GitHub @-mention: an @ not preceded by a word
// character (so e-mail addresses do not count), then a login.
var mentionLoginRe = regexp.MustCompile(`(?:^|[^\w@/.-])@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:/[A-Za-z0-9_.-]+)?)`)

// extractMentionLogins returns the distinct logins (or org/team slugs)
// @-mentioned in body, in order of first appearance.
func extractMentionLogins(body string) []string {
	if !strings.Contains(body, "@") {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range mentionLoginRe.FindAllStringSubmatch(body, -1) {
		login := strings.ToLower(m[1])
		if !seen[login] {
			seen[login] = true
			out = append(out, login)
		}
	}
	return out
}

// attributeNotificationWrite walks the caller's stack to the first
// inventoried producer. Passthrough wrappers (CreateIssueCommentBudgeted,
// PostNoteOrFold, forge routers) are skipped in favour of their caller; if
// no producer is found the outermost passthrough seen is used, and failing
// that the write is "unknown"/unclassified.
func attributeNotificationWrite() (string, NotificationWriteSite) {
	var pcs [64]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	fallback := ""
	var fallbackSite NotificationWriteSite
	for {
		f, more := frames.Next()
		name := notificationProducerName(f.Function)
		if site, ok := notificationWriteSites[name]; ok {
			if !site.Passthrough {
				return name, site
			}
			fallback, fallbackSite = name, site
		}
		if !more {
			break
		}
	}
	if fallback != "" {
		return fallback, fallbackSite
	}
	return "unknown", NotificationWriteSite{Class: NotificationClassUnclassified}
}

// notificationProducerName turns a runtime function name
// ("github.com/hivecommons/hive/pkg/github.(*Client).foo.func1") into the
// inventory key ("github.(*Client).foo"): package name, receiver, function,
// with generic instantiations and closure suffixes removed.
func notificationProducerName(fn string) string {
	for {
		i := strings.IndexByte(fn, '[')
		if i < 0 {
			break
		}
		j := strings.IndexByte(fn[i:], ']')
		if j < 0 {
			break
		}
		fn = fn[:i] + fn[i+j+1:]
	}
	if i := strings.LastIndexByte(fn, '/'); i >= 0 {
		fn = fn[i+1:]
	}
	parts := strings.Split(fn, ".")
	for len(parts) > 2 && isClosureNamePart(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, ".")
}

func isClosureNamePart(p string) bool {
	for _, prefix := range []string{"func", "gowrap", "deferwrap"} {
		if rest, ok := strings.CutPrefix(p, prefix); ok && rest != "" && isDigits(rest) {
			return true
		}
	}
	return p != "" && isDigits(p)
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
