package rfcnotify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// ReconcileLookback is how many of the newest channel messages one
	// reconciliation reads. It never reads further back.
	ReconcileLookback = 50
	// maxPostTries, maxThreadTries and maxBacklinkTries bound the attempts of
	// each step across polls and restarts; after them the step is given up
	// with a warning.
	maxPostTries     = 3
	maxThreadTries   = 3
	maxBacklinkTries = 3
	// nonceLen is Discord's longest message nonce.
	nonceLen = 25
	// threadNameLen is Discord's longest thread name, in characters.
	threadNameLen = 100
)

// Message is one message from the announcement channel's history.
type Message struct {
	ID      string
	Content string
	// Nonce is the nonce the sender attached, when Discord returned it.
	Nonce string
	// Self is true when the announcing bot authored the message.
	Self bool
}

// Threads is the Discord surface of threads mode, bound to one channel.
type Threads interface {
	// Post sends the opened card to the channel with an enforced nonce and
	// returns its message ID.
	Post(content, nonce string) (messageID string, err error)
	// StartThread opens a thread on a card and returns the thread ID.
	StartThread(messageID, name string) (threadID string, err error)
	// Send posts into a thread.
	Send(threadID, content string) error
	// Echo posts one line to the channel itself.
	Echo(content string) error
	// Recent returns up to limit of the channel's newest messages.
	Recent(limit int) ([]Message, error)
	// ThreadURL is the link to a thread.
	ThreadURL(threadID string) (string, error)
}

// Commenter is an optional extension of Source that comments on a GitHub
// issue or pull request, for the opt-in backlink.
type Commenter interface {
	Comment(ctx context.Context, repo string, number int, body string) error
}

// announceNonce is the deterministic write-ahead nonce of an RFC's opened
// card: a hash of "repo#number" plus "opened", at most nonceLen characters.
func announceNonce(key string) string {
	sum := sha256.Sum256([]byte(key + "opened"))
	return hex.EncodeToString(sum[:])[:nonceLen]
}

// threadName is "#N" plus the title, cut to Discord's thread-name limit.
func threadName(is Issue) string {
	title := strings.TrimSpace(is.Title)
	if d := strings.TrimSpace(is.DocTitle); d != "" {
		title = d
	}
	r := []rune(fmt.Sprintf("#%d %s", is.Number, title))
	if len(r) > threadNameLen {
		r = r[:threadNameLen]
	}
	return strings.TrimSpace(string(r))
}

// threadRun is the threads-mode half of one Run. base is the state to
// checkpoint into mid-batch (the pre-poll state plus what was delivered);
// nil means persist the whole state, which is right outside a diff.
type threadRun struct {
	s    *Sweeper
	res  *Result
	base *State
}

// persist saves the record of key; an error means the write-ahead guarantee
// does not hold and the caller must not go on to call Discord.
func (t *threadRun) persist(key string) error {
	if t.s.opts.StatePath == "" {
		return nil
	}
	if t.base == nil {
		return t.s.save()
	}
	return t.s.det.checkpoint(t.s.opts.StatePath, t.base, key)
}

// deliver announces one event. keep reports that the event's record, with
// its saved intent, must survive the failure described by err.
func (t *threadRun) deliver(ev Event, msg string) (keep bool, err error) {
	key := ev.Issue.Key()
	if ev.Kind == KindOpened {
		rec := t.s.det.state.Issues[key]
		if rec == nil {
			return false, t.s.threads.Echo(msg)
		}
		if rec.AnnounceState == "" {
			rec.Nonce, rec.AnnounceState = announceNonce(key), StatePending
			rec.OpenText, rec.ThreadName = msg, threadName(ev.Issue)
			if err := t.persist(key); err != nil {
				return false, fmt.Errorf("save announcement intent: %w", err)
			}
		}
		if rec.AnnounceState == StatePending {
			if err := t.postCard(key, rec); err != nil {
				return true, err
			}
		}
		if rec.MessageID != "" && rec.ThreadID == "" {
			if err := t.startThread(key, rec); err != nil {
				t.res.Errors++
				t.s.opts.Logger.Warn("rfc notify: thread not started; retrying later", "issue", key, "error", err)
			}
		}
		return false, t.persist(key)
	}

	target := t.s.det.state.Issues[ev.Thread]
	if target == nil || target.ThreadID == "" {
		// Announced before threads existed, or the thread is not up yet:
		// the plain channel line, never a back-filled thread.
		if err := t.s.threads.Echo(msg); err != nil {
			return false, err
		}
		return false, t.persist(key)
	}
	if err := t.s.threads.Send(target.ThreadID, msg); err != nil {
		return false, err
	}
	if ev.Kind == KindClosed || ev.Kind == KindReady {
		if err := t.s.threads.Echo(msg); err != nil {
			t.res.Errors++
			t.s.opts.Logger.Warn("rfc notify: channel echo failed", "issue", key, "kind", ev.Kind, "error", err)
		}
	}
	return false, t.persist(key)
}

// postCard sends the opened card with the record's nonce. The attempt is
// counted and saved first so a crash cannot make the retries unbounded.
func (t *threadRun) postCard(key string, rec *issueRecord) error {
	rec.PostTries++
	if err := t.persist(key); err != nil {
		return fmt.Errorf("save post attempt: %w", err)
	}
	id, err := t.s.threads.Post(rec.OpenText, rec.Nonce)
	if err != nil {
		if rec.PostTries >= maxPostTries {
			t.s.opts.Logger.Warn("rfc notify: giving up on the opened card", "issue", key, "tries", rec.PostTries)
		}
		return err
	}
	rec.MessageID, rec.AnnounceState = id, StatePosted
	return t.persist(key)
}

// startThread opens the RFC's thread on its confirmed card.
func (t *threadRun) startThread(key string, rec *issueRecord) error {
	rec.ThreadTries++
	if err := t.persist(key); err != nil {
		return fmt.Errorf("save thread attempt: %w", err)
	}
	id, err := t.s.threads.StartThread(rec.MessageID, rec.ThreadName)
	if err != nil {
		if rec.ThreadTries >= maxThreadTries {
			t.s.opts.Logger.Warn("rfc notify: giving up on the thread", "issue", key, "tries", rec.ThreadTries)
		}
		return err
	}
	rec.ThreadID, rec.AnnounceState = id, StateThreaded
	t.res.Threads++
	return t.persist(key)
}

// reconcile settles records a crash or a failed call left without a card or
// a thread. It does one bounded history lookup, only when a record has no
// confirmed card, and only for records this hive started announcing (those
// carrying an AnnounceState), so it never backfills. A found card is
// adopted; a missing one is re-posted with the same nonce, within the retry
// bound; an unreadable history re-posts nothing.
func (s *Sweeper) reconcile(res *Result) {
	var keys []string
	for key, rec := range s.det.state.Issues {
		if (rec.AnnounceState == StatePending || rec.AnnounceState == StatePosted) && rec.ThreadID == "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	t := &threadRun{s: s, res: res}
	var recent []Message
	lookedUp, lookupOK := false, false
	for _, key := range keys {
		rec := s.det.state.Issues[key]
		if rec.MessageID == "" {
			if !lookedUp {
				lookedUp = true
				var err error
				if recent, err = s.threads.Recent(ReconcileLookback); err != nil {
					res.Errors++
					s.opts.Logger.Warn("rfc notify: history lookup failed; not re-posting", "error", err)
				} else {
					lookupOK = true
				}
			}
			if !lookupOK {
				continue
			}
			if m, ok := findCard(recent, rec); ok {
				rec.MessageID, rec.AnnounceState = m.ID, StatePosted
				res.Reconciled++
				s.opts.Logger.Info("rfc notify: adopted existing card", "issue", key, "message", m.ID)
				if err := t.persist(key); err != nil {
					s.opts.Logger.Warn("rfc notify: save failed", "issue", key, "error", err)
					continue
				}
			} else if rec.PostTries < maxPostTries {
				if err := t.postCard(key, rec); err != nil {
					res.Errors++
					s.opts.Logger.Warn("rfc notify: re-post failed", "issue", key, "error", err)
					continue
				}
			} else {
				continue
			}
		}
		if rec.MessageID != "" && rec.ThreadID == "" && rec.ThreadTries < maxThreadTries {
			if err := t.startThread(key, rec); err != nil {
				res.Errors++
				s.opts.Logger.Warn("rfc notify: thread not started; retrying later", "issue", key, "error", err)
			}
		}
	}
}

// findCard returns the bot's own message that is rec's opened card: same
// nonce, or exactly the card text (Discord does not always echo nonces in
// history).
func findCard(recent []Message, rec *issueRecord) (Message, bool) {
	for _, m := range recent {
		if !m.Self {
			continue
		}
		if (rec.Nonce != "" && m.Nonce == rec.Nonce) || (rec.OpenText != "" && m.Content == rec.OpenText) {
			return m, true
		}
	}
	return Message{}, false
}

// backlinks posts the opt-in GitHub comment for every threaded RFC that has
// not had one, bounded by maxBacklinkTries. The attempt is counted and saved
// before the comment goes out.
func (s *Sweeper) backlinks(ctx context.Context, src Source, res *Result) {
	if !s.opts.Backlink {
		return
	}
	cm, ok := src.(Commenter)
	if !ok {
		return
	}
	var keys []string
	for key, rec := range s.det.state.Issues {
		if rec.AnnounceState == StateThreaded && rec.ThreadID != "" && !rec.BacklinkPosted && rec.BacklinkTries < maxBacklinkTries {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		rec := s.det.state.Issues[key]
		repo, num, found := strings.Cut(key, "#")
		n, err := strconv.Atoi(num)
		if !found || err != nil {
			continue
		}
		rec.BacklinkTries++
		if err := s.save(); err != nil {
			s.opts.Logger.Warn("rfc notify: save failed; backlink not posted", "issue", key, "error", err)
			rec.BacklinkTries--
			continue
		}
		url, err := s.threads.ThreadURL(rec.ThreadID)
		if err == nil {
			err = cm.Comment(ctx, repo, n, "Discussion thread: "+url)
		}
		if err != nil {
			res.Errors++
			s.opts.Logger.Warn("rfc notify: backlink comment failed", "issue", key, "tries", rec.BacklinkTries, "error", err)
			continue
		}
		rec.BacklinkPosted = true
		res.Backlinks++
		// Save at once so a crash cannot repeat the comment; the flag stays
		// set even if this write fails.
		if err := s.save(); err != nil {
			s.opts.Logger.Warn("rfc notify: save failed after backlink", "issue", key, "error", err)
		}
	}
}
