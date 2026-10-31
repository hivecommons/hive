package rfcnotify

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	// DefaultInterval is how often a hive polls for RFC transitions.
	DefaultInterval = 3 * time.Minute
	// DefaultSlack is re-read behind the watermark so an update that
	// landed while the previous poll ran is not missed; the per-issue
	// records make the overlap idempotent.
	DefaultSlack = 2 * time.Minute
)

// Source lists issues. ListOpen returns every open issue (the seed);
// ListUpdatedSince returns every issue, open or closed, updated at or after
// since.
type Source interface {
	ListOpen(ctx context.Context) ([]Issue, error)
	ListUpdatedSince(ctx context.Context, since time.Time) ([]Issue, error)
}

// PRSource is an optional extension of Source that lists pull requests,
// open, closed or merged, updated at or after since, with Issue.IsPR set.
// The sweeper asks for them only in rich mode, so line mode stays
// issues-only.
type PRSource interface {
	ListPRsUpdatedSince(ctx context.Context, since time.Time) ([]Issue, error)
}

// Publisher sends messages or summary cards to the configured channel.
type Publisher interface {
	Post(content string) error
	PostCard(content string, embed Embed) (string, error)
}

// Poster sends one message to the configured channel.
type Poster func(content string) error

// Post implements Publisher for Poster.
func (p Poster) Post(content string) error {
	return p(content)
}

// PostCard implements Publisher for Poster by falling back to Post. The
// sweeper always passes the formatted line as content; the embed.Title
// fallback only serves direct callers that pass none.
func (p Poster) PostCard(content string, embed Embed) (string, error) {
	if content != "" {
		return "", p(content)
	}
	return "", p(embed.Title)
}

// Values for Options.Format.
const (
	FormatLine = "line"
	FormatCard = "card"
)

// Rich reports whether a rich mode (card format or threads) is configured.
func (o Options) Rich() bool { return o.Format == FormatCard || o.Threads }

// Options tune one Sweeper.
type Options struct {
	// StatePath persists the detector between restarts. Empty keeps the
	// state in memory only.
	StatePath string
	// Interval between polls; non-positive means DefaultInterval.
	Interval time.Duration
	// Slack re-read behind the watermark; non-positive means DefaultSlack.
	Slack time.Duration
	// IncludeRepo prefixes issue numbers with their repo.
	IncludeRepo bool
	// Format is FormatLine (or empty, the default) or FormatCard. FormatCard
	// renders transitions as Discord summary cards (embeds).
	Format string
	// Threads opts into one thread per RFC (rich mode).
	Threads bool
	// Announcer marks this hive as the designated announcer for the
	// channel. Rich mode posts nothing without it; line mode ignores it.
	Announcer bool
	// Backlink opts into one GitHub comment linking the RFC's Discord thread,
	// posted only after the thread exists and at most once per RFC. It needs
	// a Source that implements Commenter.
	Backlink bool
	// IncludePrivate authorizes posting issues whose Private is true.
	IncludePrivate bool
	Logger         *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Result summarises one Run.
type Result struct {
	// Skipped is true when the interval had not elapsed.
	Skipped bool
	// Seeded is true when this run took the baseline instead of diffing.
	Seeded bool
	// Posted lists the messages sent, in order; Events are the matching
	// transitions.
	Posted []string
	Events []Event
	// Errors counts failed posts; the failed events stay unannounced and
	// are retried on the next poll because the state is not advanced for
	// them.
	Errors int
	// Withheld counts events not posted by a gate (private repository, or
	// rich mode without the announcer). Their state still advances, so
	// they are never replayed later.
	Withheld int
	// Unresolved counts events whose repository visibility could not be
	// determined; they stay unannounced and are retried on the next poll.
	Unresolved int
	// Threads counts Discord threads started, Reconciled counts cards
	// adopted from channel history after a crash, and Backlinks counts GitHub
	// backlink comments posted (threads mode).
	Threads, Reconciled, Backlinks int
}

// Sweeper owns the detector and its cadence.
type Sweeper struct {
	opts    Options
	det     *Detector
	lastRun time.Time
	// warnedAnnouncer makes the rich-mode announcer warning log once.
	warnedAnnouncer bool
	// threads is the Discord surface of threads mode; see SetThreads.
	threads Threads
}

// NewSweeper loads the state at opts.StatePath (an unreadable file logs
// and starts fresh, which re-seeds silently rather than re-announcing).
func NewSweeper(opts Options) *Sweeper {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Slack <= 0 {
		opts.Slack = DefaultSlack
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	det := NewDetector()
	if opts.StatePath != "" {
		loaded, err := Load(opts.StatePath)
		if err != nil {
			opts.Logger.Warn("rfc notify: state unreadable; starting fresh", "path", opts.StatePath, "error", err)
		} else {
			det = loaded
		}
	}
	return &Sweeper{opts: opts, det: det}
}

// SetThreads attaches the Discord surface threads mode posts through.
// Without it, Options.Threads falls back to the one-line poster.
func (s *Sweeper) SetThreads(t Threads) { s.threads = t }

// threadsActive reports whether this run owns Discord threads: the option,
// the designated-announcer gate and a Discord surface are all present.
func (s *Sweeper) threadsActive() bool {
	return s.opts.Threads && s.opts.Announcer && s.threads != nil
}

// Detector exposes the sweeper's detector for inspection.
func (s *Sweeper) Detector() *Detector { return s.det }

// Run polls once if the interval has elapsed, posts each event, and saves
// the state. A post failure stops the batch so the remaining events are
// retried next time rather than reordered.
func (s *Sweeper) Run(ctx context.Context, src Source, pub Publisher) (Result, error) {
	var res Result
	if src == nil || pub == nil {
		return res, fmt.Errorf("rfc notify: source and poster are required")
	}
	now := s.opts.Now()
	if !s.lastRun.IsZero() && now.Sub(s.lastRun) < s.opts.Interval {
		res.Skipped = true
		return res, nil
	}
	s.lastRun = now
	if s.opts.Rich() && !s.opts.Announcer && !s.warnedAnnouncer {
		s.opts.Logger.Warn("rfc notify: rich mode requires rfc_announcer; not posting")
		s.warnedAnnouncer = true
	}

	if !s.det.Seeded() {
		open, err := src.ListOpen(ctx)
		if err != nil {
			return res, fmt.Errorf("rfc notify: seed: %w", err)
		}
		s.det.Seed(open, now)
		res.Seeded = true
		return res, s.save()
	}

	if s.threadsActive() {
		// Settle cards and threads a crash or a failed call left half-done
		// before diffing, so they never wait on a new transition.
		s.reconcile(&res)
	}

	since := s.det.Watermark().Add(-s.opts.Slack)
	issues, err := src.ListUpdatedSince(ctx, since)
	if err != nil {
		return res, fmt.Errorf("rfc notify: list since %s: %w", since.Format(time.RFC3339), err)
	}
	if ps, ok := src.(PRSource); ok && s.opts.Rich() {
		prs, err := ps.ListPRsUpdatedSince(ctx, since)
		if err != nil {
			return res, fmt.Errorf("rfc notify: list pull requests since %s: %w", since.Format(time.RFC3339), err)
		}
		issues = append(issues, prs...)
	}
	// Diff against a copy first so a failed post leaves the state where
	// the event will be seen again next poll.
	before := s.det.clone()
	events := s.det.Observe(issues, now)
	if s.opts.Rich() && !s.opts.Announcer {
		// Not the designated announcer: record the transitions so turning
		// the announcer on later does not backfill them, but post nothing.
		res.Withheld = len(events)
		return res, s.save()
	}
	var tr *threadRun
	if s.threadsActive() {
		base := cloneState(before)
		tr = &threadRun{s: s, res: &res, base: &base}
	}
	for i, ev := range events {
		if ev.Issue.VisibilityUnknown && !s.opts.IncludePrivate {
			res.Unresolved++
			s.opts.Logger.Debug("rfc notify: repository visibility unknown; retrying next poll", "issue", ev.Issue.Key(), "kind", ev.Kind)
			s.det.rollback(before, events[i:i+1])
			continue
		}
		if ev.Issue.Private && !s.opts.IncludePrivate {
			res.Withheld++
			s.opts.Logger.Debug("rfc notify: private repository; not posting", "issue", ev.Issue.Key(), "kind", ev.Kind)
			continue
		}
		msg := Format(ev, s.opts.IncludeRepo)
		if tr != nil {
			if keep, err := tr.deliver(ev, msg); err != nil {
				res.Errors++
				s.opts.Logger.Warn("rfc notify: post failed", "issue", ev.Issue.Key(), "kind", ev.Kind, "error", err)
				// A card whose write-ahead intent is saved is retried by
				// reconciliation, so only the events after it are replayed.
				if keep {
					s.det.rollback(before, events[i+1:])
				} else {
					s.det.rollback(before, events[i:])
				}
				break
			}
		} else if err := s.publish(pub, ev, msg); err != nil {
			res.Errors++
			s.opts.Logger.Warn("rfc notify: post failed", "issue", ev.Issue.Key(), "kind", ev.Kind, "error", err)
			// Keep what was announced, forget what was not: replay the
			// snapshot for the unposted issues on top of the old records.
			s.det.rollback(before, events[i:])
			break
		}
		res.Posted = append(res.Posted, msg)
		res.Events = append(res.Events, ev)
	}
	if tr != nil {
		s.backlinks(ctx, src, &res)
	}
	return res, s.save()
}

// publish sends one event outside threads mode: a summary card when the
// format is card and the body carries RFC metadata, else the plain line.
func (s *Sweeper) publish(pub Publisher, ev Event, msg string) error {
	if s.opts.Format == FormatCard {
		if meta := ParseRFC(ev.Issue.Body); !meta.Empty() {
			_, err := pub.PostCard(msg, Card(ev, meta, s.opts.IncludeRepo))
			return err
		}
	}
	return pub.Post(msg)
}

func (s *Sweeper) save() error {
	if s.opts.StatePath == "" {
		return nil
	}
	return s.det.Save(s.opts.StatePath)
}
