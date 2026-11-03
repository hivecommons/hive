package prstatus

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeForge keeps one item's comments in memory. beforeList runs before
// every ListComments (with the 1-based call number) so a test can change
// the item between the writer's read and its edit; inFlight/overlap prove
// the per-item lock keeps two writers from interleaving.
type fakeForge struct {
	mu       sync.Mutex
	comments []Comment
	nextID   int64
	lists    int
	edits    int
	creates  int
	deleted  []int64

	beforeList   func(n int, f *fakeForge)
	beforeCreate func(f *fakeForge)

	inFlight atomic.Int32
	overlap  atomic.Bool
	pause    time.Duration
}

func (f *fakeForge) enter() func() {
	if f.inFlight.Add(1) > 1 {
		f.overlap.Store(true)
	}
	if f.pause > 0 {
		<-time.After(f.pause)
	}
	return func() { f.inFlight.Add(-1) }
}

func (f *fakeForge) add(body string, own bool) int64 {
	f.nextID++
	f.comments = append(f.comments, Comment{ID: f.nextID, Body: body, Own: own})
	return f.nextID
}

func (f *fakeForge) ListComments(ctx context.Context) ([]Comment, error) {
	defer f.enter()()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.beforeList != nil {
		f.beforeList(f.lists, f)
	}
	return append([]Comment(nil), f.comments...), nil
}

func (f *fakeForge) EditComment(ctx context.Context, id int64, body string) error {
	defer f.enter()()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.comments {
		if f.comments[i].ID == id {
			f.comments[i].Body = body
			f.edits++
			return nil
		}
	}
	return errors.New("no such comment")
}

func (f *fakeForge) CreateComment(ctx context.Context, body string) error {
	defer f.enter()()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beforeCreate != nil {
		f.beforeCreate(f)
	}
	f.creates++
	f.add(body, true)
	return nil
}

func (f *fakeForge) DeleteComment(ctx context.Context, id int64) error {
	defer f.enter()()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.comments {
		if f.comments[i].ID == id {
			f.comments = append(f.comments[:i], f.comments[i+1:]...)
			f.deleted = append(f.deleted, id)
			return nil
		}
	}
	return errors.New("no such comment")
}

func (f *fakeForge) statusBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments {
		if IsStatusComment(c.Body) {
			out = append(out, c.Body)
		}
	}
	return out
}

func sectionCount(body, key string) int {
	n := 0
	for _, s := range Parse(body) {
		if s.Key == key {
			n++
		}
	}
	return n
}

// Two writers upserting different sections of one item at the same time
// both land, in one status comment, and their calls never interleave.
func TestWriteConcurrentWritersDifferentKeysBothSurvive(t *testing.T) {
	f := &fakeForge{pause: 2 * time.Millisecond}
	f.add(Upsert("", "seed", "seed"), true)
	item := ItemKey("o/r", 1)
	keys := []string{"a", "b", "c", "d"}
	var wg sync.WaitGroup
	errs := make(chan error, len(keys))
	for _, k := range keys {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			_, err := Write(context.Background(), f, item, k, "text "+k)
			errs <- err
		}(k)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.overlap.Load() {
		t.Fatal("two status writers on one item interleaved their calls")
	}
	bodies := f.statusBodies()
	if len(bodies) != 1 {
		t.Fatalf("want one status comment, got %d", len(bodies))
	}
	for _, k := range append([]string{"seed"}, keys...) {
		if sectionCount(bodies[0], k) != 1 {
			t.Fatalf("section %q lost or duplicated:\n%s", k, bodies[0])
		}
	}
}

// Two writers racing to create the status comment on an item that has
// none: one creates it, the other edits it; nothing is posted twice.
func TestWriteConcurrentCreateMakesOneComment(t *testing.T) {
	f := &fakeForge{pause: time.Millisecond}
	item := ItemKey("o/r", 2)
	var wg sync.WaitGroup
	var created atomic.Int32
	for _, k := range []string{"a", "b"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			res, err := Write(context.Background(), f, item, k, "text "+k)
			if err != nil {
				t.Error(err)
			}
			if res.Created {
				created.Add(1)
			}
		}(k)
	}
	wg.Wait()
	bodies := f.statusBodies()
	if f.creates != 1 || created.Load() != 1 || len(bodies) != 1 {
		t.Fatalf("creates=%d reported=%d status comments=%d, want exactly one", f.creates, created.Load(), len(bodies))
	}
	if sectionCount(bodies[0], "a") != 1 || sectionCount(bodies[0], "b") != 1 {
		t.Fatalf("a section was lost:\n%s", bodies[0])
	}
}

// Another writer (another hive instance) adds a section between Write's
// read and its edit: the pre-edit re-read sees the change, the section is
// recomputed on the fresh body, and the third-party section survives.
func TestWriteRetriesWhenBodyChangedBetweenReadAndEdit(t *testing.T) {
	f := &fakeForge{}
	id := f.add(Upsert("", "mine", "old"), true)
	f.beforeList = func(n int, f *fakeForge) {
		if n == 2 {
			for i := range f.comments {
				if f.comments[i].ID == id {
					f.comments[i].Body = Upsert(f.comments[i].Body, "theirs", "third-party")
				}
			}
		}
	}
	res, err := Write(context.Background(), f, ItemKey("o/r", 3), "mine", "new")
	if err != nil || !res.Edited || res.Created {
		t.Fatalf("Write = %+v, %v", res, err)
	}
	if f.lists < 3 {
		t.Fatalf("changed body must trigger a re-read and retry, lists=%d", f.lists)
	}
	if f.edits != 1 {
		t.Fatalf("edits=%d, want 1", f.edits)
	}
	body := f.statusBodies()[0]
	if got, _ := Get(body, "theirs"); got != "third-party" {
		t.Fatalf("third-party section lost:\n%s", body)
	}
	if got, _ := Get(body, "mine"); got != "new" {
		t.Fatalf("own section not written:\n%s", body)
	}
}

// A comment that changes under every attempt is reported, not overwritten.
func TestWriteGivesUpAfterMaxAttempts(t *testing.T) {
	f := &fakeForge{}
	id := f.add(Upsert("", "mine", "old"), true)
	f.beforeList = func(n int, f *fakeForge) {
		for i := range f.comments {
			if f.comments[i].ID == id {
				f.comments[i].Body = Upsert(f.comments[i].Body, "churn", strings.Repeat("x", n))
			}
		}
	}
	if _, err := Write(context.Background(), f, ItemKey("o/r", 4), "mine", "new"); !errors.Is(err, ErrStatusConflict) {
		t.Fatalf("err = %v, want ErrStatusConflict", err)
	}
	if f.edits != 0 {
		t.Fatalf("a conflicting comment was edited %d times", f.edits)
	}
}

// Two status comments: every section is merged into the oldest, the newer
// hive-authored copy is deleted, and each section appears exactly once.
func TestWriteMergesDuplicateStatusCommentsIntoOldest(t *testing.T) {
	f := &fakeForge{}
	f.add("unrelated comment", false)
	oldest := f.add(Upsert(Upsert("", "a", "a-oldest"), "shared", "s-oldest"), true)
	newer := f.add(Upsert(Upsert("", "b", "b-newer"), "shared", "s-newer"), true)
	res, err := Write(context.Background(), f, ItemKey("o/r", 5), "c", "c-text")
	if err != nil || res.Created || res.Deleted != 1 {
		t.Fatalf("Write = %+v, %v", res, err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != newer {
		t.Fatalf("deleted %v, want the newer status comment %d", f.deleted, newer)
	}
	bodies := f.statusBodies()
	if len(bodies) != 1 {
		t.Fatalf("want one status comment left, got %d", len(bodies))
	}
	body := bodies[0]
	if f.comments[1].ID != oldest {
		t.Fatalf("the oldest status comment must be the survivor")
	}
	for _, k := range []string{"a", "b", "shared", "c"} {
		if sectionCount(body, k) != 1 {
			t.Fatalf("section %q not exactly once:\n%s", k, body)
		}
	}
	if got, _ := Get(body, "shared"); got != "s-oldest" {
		t.Fatalf("the oldest comment's copy of a shared section must win, got %q", got)
	}
}

// Another instance creates a status comment while this one is creating
// its own: the post-create reconciliation folds this one's section into
// the older comment and deletes the newer one.
func TestWriteReconcilesDuplicateCreate(t *testing.T) {
	f := &fakeForge{}
	f.beforeCreate = func(f *fakeForge) {
		f.add(Upsert("", "other", "other-instance"), true)
	}
	res, err := Write(context.Background(), f, ItemKey("o/r", 6), "mine", "mine-text")
	if err != nil || !res.Created || res.Deleted != 1 {
		t.Fatalf("Write = %+v, %v", res, err)
	}
	bodies := f.statusBodies()
	if len(bodies) != 1 {
		t.Fatalf("duplicate status comment left behind: %d", len(bodies))
	}
	if sectionCount(bodies[0], "other") != 1 || sectionCount(bodies[0], "mine") != 1 {
		t.Fatalf("sections not merged:\n%s", bodies[0])
	}
}

// A status marker in a comment a person wrote is merged from but never
// deleted.
func TestWriteNeverDeletesForeignStatusComment(t *testing.T) {
	f := &fakeForge{}
	f.add(Upsert("", "a", "a"), true)
	f.add(Upsert("", "pasted", "by a person"), false)
	res, err := Write(context.Background(), f, ItemKey("o/r", 7), "b", "b")
	if err != nil || res.Deleted != 0 || len(f.deleted) != 0 {
		t.Fatalf("Write = %+v, %v, deleted=%v", res, err, f.deleted)
	}
	if got, _ := Get(f.comments[0].Body, "pasted"); got != "by a person" {
		t.Fatalf("foreign section not merged into the oldest:\n%s", f.comments[0].Body)
	}
}

// A status marker a person pasted is never the edit target, even when it is
// the oldest status comment: the oldest hive-owned one is edited instead.
func TestWriteEditsOldestOwnedNotForeignStatusComment(t *testing.T) {
	f := &fakeForge{}
	foreignBody := Upsert("", "pasted", "by a person")
	foreign := f.add(foreignBody, false)
	own := f.add(Upsert("", "a", "a"), true)
	res, err := Write(context.Background(), f, ItemKey("o/r", 9), "b", "b")
	if err != nil || res.Created || !res.Edited || res.Deleted != 0 {
		t.Fatalf("Write = %+v, %v", res, err)
	}
	for _, c := range f.comments {
		switch c.ID {
		case foreign:
			if c.Body != foreignBody {
				t.Fatalf("foreign comment was rewritten:\n%s", c.Body)
			}
		case own:
			if sectionCount(c.Body, "b") != 1 || sectionCount(c.Body, "a") != 1 {
				t.Fatalf("owned comment not updated:\n%s", c.Body)
			}
		}
	}
}

// With only a foreign status comment the writer creates its own, merging
// from the foreign one and leaving it in place.
func TestWriteCreatesOwnedCommentWhenOnlyForeignStatusExists(t *testing.T) {
	f := &fakeForge{}
	foreignBody := Upsert("", "pasted", "by a person")
	f.add(foreignBody, false)
	res, err := Write(context.Background(), f, ItemKey("o/r", 10), "b", "b")
	if err != nil || !res.Created || res.Edited || res.Deleted != 0 {
		t.Fatalf("Write = %+v, %v", res, err)
	}
	if len(f.comments) != 2 || f.comments[0].Body != foreignBody {
		t.Fatalf("foreign comment must be left untouched: %+v", f.comments)
	}
	got := f.comments[1].Body
	if !f.comments[1].Own || sectionCount(got, "b") != 1 || sectionCount(got, "pasted") != 1 {
		t.Fatalf("new owned comment wrong: %+v", f.comments[1])
	}
}

func TestWriteEmptyTextWithoutCommentWritesNothing(t *testing.T) {
	f := &fakeForge{}
	res, err := Write(context.Background(), f, ItemKey("o/r", 8), "a", "  ")
	if err != nil || res != (WriteResult{}) || f.creates != 0 {
		t.Fatalf("Write = %+v, %v, creates=%d", res, err, f.creates)
	}
}

// Write is the only hive:status writer. Every other non-test Go file in the
// tree may read a status comment but must not build one: rendering or
// upserting a status body, or naming the marker, outside this package is a
// second writer that bypasses the lock, the compare-and-retry and the
// duplicate reconciliation.
func TestNoStatusWriterOutsideWrite(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"prstatus.Marker",
		"prstatus.Upsert(",
		"prstatus.Render(",
		"prstatus.Merge(",
		Marker,
		"hive:status -->",
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			if path == self {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, s := range forbidden {
			if strings.Contains(string(raw), s) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+": "+s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("hive:status comments must be written through prstatus.Write only:\n%s", strings.Join(offenders, "\n"))
	}
}

func TestWriteNeutralisesIncidentalMentions(t *testing.T) {
	f := &fakeForge{}
	text := "Title: fix for @someuser's bug, contact bob@example.com"
	if _, err := Write(context.Background(), f, "o/r#1", "k", text); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	body := f.comments[0].Body
	if strings.Contains(body, "@someuser") {
		t.Fatalf("live mention survived: %q", body)
	}
	if !strings.Contains(body, "`someuser`") {
		t.Fatalf("login not preserved readably: %q", body)
	}
	if !strings.Contains(body, "bob@example.com") {
		t.Fatalf("email address was altered: %q", body)
	}
}

func TestWriteMentioningKeepsDeliberateMention(t *testing.T) {
	f := &fakeForge{}
	if _, err := WriteMentioning(context.Background(), f, "o/r#1", "k", "@author this PR is held"); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0].Body, "@author this PR is held") {
		t.Fatalf("deliberate mention must survive WriteMentioning: %+v", f.comments)
	}
}

func TestWriteNeutralisesMergedForeignStatusBodies(t *testing.T) {
	// Create path: the only status comment is foreign, so its sections are
	// merged into the new hive-owned comment.
	f := &fakeForge{}
	f.add(Upsert("", "theirs", "see @victim for details"), false)
	if _, err := Write(context.Background(), f, "o/r#1", "k", "ours"); err != nil {
		t.Fatal(err)
	}
	var own []Comment
	for _, c := range f.comments {
		if c.Own {
			own = append(own, c)
		}
	}
	if len(own) != 1 {
		t.Fatalf("own comments = %d, want 1", len(own))
	}
	if strings.Contains(own[0].Body, "@victim") {
		t.Fatalf("foreign mention went live in the hive-owned comment: %q", own[0].Body)
	}
	if got, _ := Get(own[0].Body, "theirs"); !strings.Contains(got, "`victim`") {
		t.Fatalf("foreign section not merged readably: %q", own[0].Body)
	}
	if strings.Contains(f.comments[0].Body, "`victim`") {
		t.Fatalf("foreign comment must not be edited: %q", f.comments[0].Body)
	}

	// Reconcile path: a foreign status comment newer than the hive-owned one
	// is merged into it.
	f = &fakeForge{}
	f.add(Upsert("", "k", "ours"), true)
	f.add(Upsert("", "theirs", "ping @victim"), false)
	if _, err := Reconcile(context.Background(), f, "o/r#1"); err != nil {
		t.Fatal(err)
	}
	if b := f.comments[0].Body; strings.Contains(b, "@victim") || !strings.Contains(b, "`victim`") {
		t.Fatalf("reconcile merged a live foreign mention: %q", b)
	}
}
