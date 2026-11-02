package prstatus

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// errForge fails the calls a test names. listErr gets the 1-based
// ListComments call number; the other errors fail every such call.
type errForge struct {
	*fakeForge
	listCalls int
	listErr   func(n int) error
	createErr error
	editErr   error
	deleteErr error
}

func (e *errForge) ListComments(ctx context.Context) ([]Comment, error) {
	e.listCalls++
	if e.listErr != nil {
		if err := e.listErr(e.listCalls); err != nil {
			return nil, err
		}
	}
	return e.fakeForge.ListComments(ctx)
}

func (e *errForge) EditComment(ctx context.Context, id int64, body string) error {
	if e.editErr != nil {
		return e.editErr
	}
	return e.fakeForge.EditComment(ctx, id, body)
}

func (e *errForge) CreateComment(ctx context.Context, body string) error {
	if e.createErr != nil {
		return e.createErr
	}
	return e.fakeForge.CreateComment(ctx, body)
}

func (e *errForge) DeleteComment(ctx context.Context, id int64) error {
	if e.deleteErr != nil {
		return e.deleteErr
	}
	return e.fakeForge.DeleteComment(ctx, id)
}

var errBoom = errors.New("boom")

func failListAt(target int) func(int) error {
	return func(n int) error {
		if n == target {
			return errBoom
		}
		return nil
	}
}

func TestParseEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []Section
	}{
		{"no sections", Marker + "\n" + Heading, nil},
		{"open without suffix", sectionOpenPrefix + "key", nil},
		{"unterminated section keeps rest", sectionOpenPrefix + " k " + sectionOpenSuffix + "\n  body \n", []Section{{Key: "k", Body: "body"}}},
		{"two sections with noise between", "x" + sectionOpenPrefix + "a" + sectionOpenSuffix + "1" + sectionClose + "junk" +
			sectionOpenPrefix + "b" + sectionOpenSuffix + "2" + sectionClose, []Section{{Key: "a", Body: "1"}, {Key: "b", Body: "2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("Parse = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Parse[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestGetMissingSection(t *testing.T) {
	body := Upsert("", "a", "x")
	if got, ok := Get(body, "nope"); ok || got != "" {
		t.Fatalf("Get = %q, %v", got, ok)
	}
	if IsStatusComment("plain") {
		t.Fatal("plain text is not a status comment")
	}
}

func TestUpsertCollapsesDuplicateKeys(t *testing.T) {
	dup := Render([]Section{{Key: "a", Body: "one"}, {Key: "b", Body: "b"}, {Key: "a", Body: "two"}})
	body := Upsert(dup, "a", "new")
	if sectionCount(body, "a") != 1 {
		t.Fatalf("duplicate key survived:\n%s", body)
	}
	if got, _ := Get(body, "a"); got != "new" {
		t.Fatalf("a = %q", got)
	}
	body = Upsert(dup, "a", "")
	if sectionCount(body, "a") != 0 || sectionCount(body, "b") != 1 {
		t.Fatalf("empty text did not remove every copy:\n%s", body)
	}
}

func TestMergeFirstOccurrenceWins(t *testing.T) {
	got := Merge(Upsert(Upsert("", "a", "old"), "b", "b"), Upsert(Upsert("", "a", "new"), "c", "c"), "no sections")
	for _, k := range []string{"a", "b", "c"} {
		if sectionCount(got, k) != 1 {
			t.Fatalf("section %q not exactly once:\n%s", k, got)
		}
	}
	if v, _ := Get(got, "a"); v != "old" {
		t.Fatalf("a = %q, want the first copy", v)
	}
}

func TestItemKeyNormalises(t *testing.T) {
	if got := ItemKey("  Owner/Repo ", 12); got != "owner/repo#12" {
		t.Fatalf("ItemKey = %q", got)
	}
}

func TestWriteNoChangeDoesNotEdit(t *testing.T) {
	f := &fakeForge{}
	f.add(Upsert("", "a", "same"), true)
	res, err := Write(context.Background(), f, ItemKey("o/r", 20), "a", "same")
	if err != nil || res != (WriteResult{}) || f.edits != 0 {
		t.Fatalf("Write = %+v, %v, edits=%d", res, err, f.edits)
	}
}

func TestWriteReleasesItemLock(t *testing.T) {
	item := ItemKey("o/r", 21)
	f := &fakeForge{}
	if _, err := Write(context.Background(), f, item, "a", "x"); err != nil {
		t.Fatal(err)
	}
	itemLocks.mu.Lock()
	_, held := itemLocks.m[item]
	itemLocks.mu.Unlock()
	if held {
		t.Fatal("lock table entry not dropped after Write")
	}
}

func TestWriteErrorPaths(t *testing.T) {
	two := func(f *fakeForge) {
		f.add(Upsert("", "a", "a"), true)
		f.add(Upsert("", "b", "b"), true)
	}
	one := func(f *fakeForge) { f.add(Upsert("", "a", "old"), true) }
	cases := []struct {
		name  string
		seed  func(*fakeForge)
		setup func(*errForge)
		want  error
	}{
		{"first list fails", one, func(e *errForge) { e.listErr = failListAt(1) }, errBoom},
		{"create fails", func(*fakeForge) {}, func(e *errForge) { e.createErr = errBoom }, errBoom},
		{"post-create reconcile list fails", func(*fakeForge) {}, func(e *errForge) { e.listErr = failListAt(2) }, errBoom},
		{"pre-edit re-read fails", one, func(e *errForge) { e.listErr = failListAt(2) }, errBoom},
		{"edit fails", one, func(e *errForge) { e.editErr = errBoom }, errBoom},
		{"comment gone before edit", one, func(e *errForge) {
			e.beforeList = func(n int, f *fakeForge) {
				if n == 2 {
					f.comments = nil
				}
			}
		}, ErrStatusCommentGone},
		{"duplicate reconcile list fails", two, func(e *errForge) { e.listErr = failListAt(2) }, errBoom},
		{"duplicate reconcile re-read fails", two, func(e *errForge) { e.listErr = failListAt(3) }, errBoom},
		{"duplicate reconcile edit fails", two, func(e *errForge) { e.editErr = errBoom }, errBoom},
		{"duplicate delete fails", two, func(e *errForge) { e.deleteErr = errBoom }, errBoom},
		{"list after reconcile fails", two, func(e *errForge) { e.listErr = failListAt(4) }, errBoom},
		{"status comment vanishes after reconcile", two, func(e *errForge) {
			e.beforeList = func(n int, f *fakeForge) {
				if n == 4 {
					f.comments = nil
				}
			}
		}, ErrStatusCommentGone},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &errForge{fakeForge: &fakeForge{}}
			tc.seed(e.fakeForge)
			tc.setup(e)
			_, err := Write(context.Background(), e, ItemKey("o/r", 100+i), "k", "v")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestWriteDeleteErrorNamesComment(t *testing.T) {
	e := &errForge{fakeForge: &fakeForge{}, deleteErr: errBoom}
	e.add(Upsert("", "a", "a"), true)
	dup := e.add(Upsert("", "b", "b"), true)
	_, err := Write(context.Background(), e, ItemKey("o/r", 30), "c", "c")
	if err == nil || !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "duplicate hive status comment") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "2") || dup != 2 {
		t.Fatalf("error should name comment %d: %v", dup, err)
	}
}

func TestReconcile(t *testing.T) {
	t.Run("none or one is a no-op", func(t *testing.T) {
		f := &fakeForge{}
		f.add("unrelated", false)
		res, err := Reconcile(context.Background(), f, ItemKey("o/r", 40))
		if err != nil || res != (WriteResult{}) {
			t.Fatalf("Reconcile = %+v, %v", res, err)
		}
		f.add(Upsert("", "a", "a"), true)
		res, err = Reconcile(context.Background(), f, ItemKey("o/r", 40))
		if err != nil || res != (WriteResult{}) || f.edits != 0 {
			t.Fatalf("Reconcile = %+v, %v, edits=%d", res, err, f.edits)
		}
	})
	t.Run("merges and deletes own duplicates only", func(t *testing.T) {
		f := &fakeForge{}
		f.add(Upsert("", "a", "a"), true)
		f.add(Upsert("", "b", "b"), true)
		f.add(Upsert("", "c", "c"), false)
		res, err := Reconcile(context.Background(), f, ItemKey("o/r", 41))
		if err != nil || !res.Edited || res.Deleted != 1 || res.Created {
			t.Fatalf("Reconcile = %+v, %v", res, err)
		}
		if len(f.comments) != 2 {
			t.Fatalf("comments left = %d, want 2", len(f.comments))
		}
		for _, k := range []string{"a", "b", "c"} {
			if sectionCount(f.comments[0].Body, k) != 1 {
				t.Fatalf("section %q not merged into the oldest:\n%s", k, f.comments[0].Body)
			}
		}
	})
	t.Run("list error", func(t *testing.T) {
		e := &errForge{fakeForge: &fakeForge{}, listErr: failListAt(1)}
		if _, err := Reconcile(context.Background(), e, ItemKey("o/r", 42)); !errors.Is(err, errBoom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("conflict leaves duplicates alone", func(t *testing.T) {
		f := &fakeForge{}
		f.add(Upsert("", "a", "a"), true)
		f.add(Upsert("", "b", "b"), true)
		f.beforeList = func(n int, f *fakeForge) {
			f.comments[0].Body = Upsert(f.comments[0].Body, "churn", strings.Repeat("x", n))
		}
		res, err := Reconcile(context.Background(), f, ItemKey("o/r", 43))
		if !errors.Is(err, ErrStatusConflict) || res.Deleted != 0 || len(f.deleted) != 0 {
			t.Fatalf("Reconcile = %+v, %v", res, err)
		}
	})
}
