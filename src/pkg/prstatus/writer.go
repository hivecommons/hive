package prstatus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/advisory"
)

// Comment is one issue/PR comment as the status writer sees it.
type Comment struct {
	ID   int64
	Body string
	// Own reports the hive authored the comment. Only an Own duplicate is
	// ever deleted by reconciliation; a status marker pasted by a person
	// is merged from but left in place.
	Own bool
}

// Forge is the comment API the status writer drives for one item. The
// GitHub client and the auto-merge engine each adapt their own transport to
// it, so both go through Write and share its per-item serialisation.
type Forge interface {
	ListComments(ctx context.Context) ([]Comment, error)
	EditComment(ctx context.Context, id int64, body string) error
	CreateComment(ctx context.Context, body string) error
	DeleteComment(ctx context.Context, id int64) error
}

// WriteResult reports what Write changed on GitHub.
type WriteResult struct {
	// Created is set when Write posted a new status comment (the only
	// write that notifies anyone).
	Created bool
	// Edited is set when Write edited a status comment in place.
	Edited bool
	// Deleted counts duplicate status comments removed by reconciliation.
	Deleted int
}

// MaxWriteAttempts bounds the compare-and-retry loop: a status comment that
// keeps changing between read and edit this many times is reported as
// ErrStatusConflict rather than overwritten.
const MaxWriteAttempts = 4

var (
	// ErrStatusConflict means the status comment changed under every
	// attempt to edit it.
	ErrStatusConflict = errors.New("hive status comment kept changing between read and edit")
	// ErrStatusCommentGone means the status comment disappeared between
	// read and edit.
	ErrStatusCommentGone = errors.New("hive status comment disappeared between read and edit")
)

// Write is the single authoritative hive:status writer: it sets section key
// of item's status comment to text (an empty text removes it), creating the
// comment only when the item has none. item names the issue or PR
// ("owner/repo#number") and keys the per-process lock every writer shares,
// so two writers on one item never interleave their read and edit.
//
// Every edit is compare-and-retry: the comment is re-read immediately
// before the edit, and when its body is no longer the one the section was
// computed from the section is recomputed on the fresh body, so a section
// another writer (or another hive) added in between survives. After a
// create, and whenever more than one status comment is found, the
// duplicates are reconciled: their sections are merged into the oldest
// hive-authored comment and the other hive-authored copies deleted. A
// comment the hive did not author is never edited or deleted.
//
// text is copied prose as far as Write is concerned: every @login in it is
// neutralised (advisory.NeutralizeMentions) so that a quoted title or
// comment never notifies the person named, and so are the bodies of
// foreign status comments merged into a hive-owned one. A section that
// deliberately mentions someone goes through WriteMentioning instead.
func Write(ctx context.Context, f Forge, item, key, text string) (WriteResult, error) {
	return write(ctx, f, item, key, advisory.NeutralizeMentions(text))
}

// WriteMentioning is Write for the one kind of section that is meant to
// notify: text is posted verbatim, so a deliberate @login in it stays live.
// The caller owns the hygiene of that text and must neutralise any copied
// prose (check names, review quotes, titles) it composed into it. Foreign
// status comments merged into the hive-owned one are still neutralised.
func WriteMentioning(ctx context.Context, f Forge, item, key, text string) (WriteResult, error) {
	return write(ctx, f, item, key, text)
}

func write(ctx context.Context, f Forge, item, key, text string) (WriteResult, error) {
	unlock := lockItem(item)
	defer unlock()
	var res WriteResult
	status, err := listStatusComments(ctx, f)
	if err != nil {
		return res, err
	}
	if len(ownStatus(status)) == 0 {
		if strings.TrimSpace(text) == "" {
			return res, nil
		}
		// Foreign status comments are merged from but never edited, so a
		// hive-owned comment is created to carry the merged sections. Their
		// text becomes part of a body the hive owns and re-edits, so any
		// live mention in it is neutralised on the way in.
		bodies := make([]string, 0, len(status))
		for _, c := range status {
			bodies = append(bodies, foreignBody(c))
		}
		if err := f.CreateComment(ctx, Upsert(Merge(bodies...), key, text)); err != nil {
			return res, err
		}
		res.Created = true
		err := reconcile(ctx, f, &res)
		return res, err
	}
	if len(status) > 1 {
		if err := reconcile(ctx, f, &res); err != nil {
			return res, err
		}
		if status, err = listStatusComments(ctx, f); err != nil {
			return res, err
		}
	}
	own := ownStatus(status)
	if len(own) == 0 {
		return res, ErrStatusCommentGone
	}
	target := own[0]
	edited, err := editCompareAndRetry(ctx, f, target.ID, target.Body, func(cur string) string {
		return Upsert(cur, key, text)
	})
	res.Edited = res.Edited || edited
	return res, err
}

// Reconcile merges every status comment on the item into the oldest
// hive-authored one and deletes the other hive-authored copies, under the same per-item lock as
// Write. It is a no-op when the item has at most one status comment.
func Reconcile(ctx context.Context, f Forge, item string) (WriteResult, error) {
	unlock := lockItem(item)
	defer unlock()
	var res WriteResult
	err := reconcile(ctx, f, &res)
	return res, err
}

func reconcile(ctx context.Context, f Forge, res *WriteResult) error {
	status, err := listStatusComments(ctx, f)
	if err != nil || len(status) < 2 {
		return err
	}
	own := ownStatus(status)
	if len(own) == 0 {
		return nil
	}
	oldest := own[0]
	var newer []Comment
	others := make([]string, 0, len(status)-1)
	for _, c := range status {
		if c.ID == oldest.ID {
			continue
		}
		newer = append(newer, c)
		others = append(others, foreignBody(c))
	}
	edited, err := editCompareAndRetry(ctx, f, oldest.ID, oldest.Body, func(cur string) string {
		return Merge(append([]string{cur}, others...)...)
	})
	res.Edited = res.Edited || edited
	if err != nil {
		return err
	}
	for _, c := range newer {
		if !c.Own {
			continue
		}
		if err := f.DeleteComment(ctx, c.ID); err != nil {
			return fmt.Errorf("deleting duplicate hive status comment %d: %w", c.ID, err)
		}
		res.Deleted++
	}
	return nil
}

// foreignBody returns c's body as it may be merged into a hive-owned
// comment: a comment the hive did not author is neutralised, since its
// mentions would otherwise notify again on every later edit of the merged
// body; a hive-authored body is already hygienic (or deliberately
// mentioning) and is taken as is.
func foreignBody(c Comment) string {
	if c.Own {
		return c.Body
	}
	return advisory.NeutralizeMentions(c.Body)
}

// ownStatus returns the hive-authored comments of status, keeping order.
func ownStatus(status []Comment) []Comment {
	var out []Comment
	for _, c := range status {
		if c.Own {
			out = append(out, c)
		}
	}
	return out
}

// editCompareAndRetry edits comment id to compute(body), where body is the
// comment as last read. Immediately before each edit the comment is read
// again; if it changed, compute runs on the fresh body and the edit is
// retried, up to MaxWriteAttempts reads.
func editCompareAndRetry(ctx context.Context, f Forge, id int64, base string, compute func(string) string) (bool, error) {
	for attempt := 1; ; attempt++ {
		next := compute(base)
		if next == base {
			return false, nil
		}
		fresh, found, err := readComment(ctx, f, id)
		if err != nil {
			return false, err
		}
		if !found {
			return false, ErrStatusCommentGone
		}
		if fresh == base {
			if err := f.EditComment(ctx, id, next); err != nil {
				return false, err
			}
			return true, nil
		}
		if attempt >= MaxWriteAttempts {
			return false, ErrStatusConflict
		}
		base = fresh
	}
}

func readComment(ctx context.Context, f Forge, id int64) (string, bool, error) {
	comments, err := f.ListComments(ctx)
	if err != nil {
		return "", false, err
	}
	for _, c := range comments {
		if c.ID == id {
			return c.Body, true, nil
		}
	}
	return "", false, nil
}

// listStatusComments returns the item's status comments, oldest first.
// GitHub comment IDs increase with creation time.
func listStatusComments(ctx context.Context, f Forge) ([]Comment, error) {
	comments, err := f.ListComments(ctx)
	if err != nil {
		return nil, err
	}
	var out []Comment
	for _, c := range comments {
		if IsStatusComment(c.Body) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Merge renders one status body holding every section of bodies, each key
// exactly once. bodies are oldest first and the first occurrence of a key
// wins: the oldest comment is the one every pass edits, so its copy of a
// section is the current one.
func Merge(bodies ...string) string {
	seen := map[string]bool{}
	var out []Section
	for _, b := range bodies {
		for _, s := range Parse(b) {
			if seen[s.Key] {
				continue
			}
			seen[s.Key] = true
			out = append(out, s)
		}
	}
	return Render(out)
}

// ItemKey is the lock key Write uses for repo#number.
func ItemKey(repo string, number int) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(strings.TrimSpace(repo)), number)
}

type itemLock struct {
	mu   sync.Mutex
	refs int
}

var itemLocks = struct {
	mu sync.Mutex
	m  map[string]*itemLock
}{m: map[string]*itemLock{}}

// lockItem serialises status writes per item within the process and
// returns the unlock. Entries are dropped once no writer holds or waits on
// them, so the table stays as small as the set of items being written.
func lockItem(item string) func() {
	itemLocks.mu.Lock()
	l := itemLocks.m[item]
	if l == nil {
		l = &itemLock{}
		itemLocks.m[item] = l
	}
	l.refs++
	itemLocks.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		itemLocks.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(itemLocks.m, item)
		}
		itemLocks.mu.Unlock()
	}
}
