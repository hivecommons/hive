package upstreamwatch

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// fakeContents is a ForkContents whose answers come from a map. A path listed
// in errs returns that error; a path present in exists returns its bool; any
// other path reports missing.
type fakeContents struct {
	exists map[string]bool
	errs   map[string]error
	calls  int
}

func (f *fakeContents) Exists(_ context.Context, path string) (bool, error) {
	f.calls++
	if err, ok := f.errs[path]; ok {
		return false, err
	}
	return f.exists[path], nil
}

func TestApplicable(t *testing.T) {
	ctx := context.Background()

	t.Run("no files is applicable", func(t *testing.T) {
		ok, present, err := Applicable(ctx, &fakeContents{}, Item{Kind: KindRelease})
		if err != nil || !ok || present != nil {
			t.Fatalf("got ok=%v present=%v err=%v", ok, present, err)
		}
	})

	t.Run("all files missing is not applicable", func(t *testing.T) {
		fc := &fakeContents{exists: map[string]bool{}}
		ok, present, err := Applicable(ctx, fc, Item{Files: []string{"a.go", "b.go"}})
		if err != nil || ok || len(present) != 0 {
			t.Fatalf("got ok=%v present=%v err=%v", ok, present, err)
		}
	})

	t.Run("some files present is applicable", func(t *testing.T) {
		fc := &fakeContents{exists: map[string]bool{"b.go": true}}
		ok, present, err := Applicable(ctx, fc, Item{Files: []string{"a.go", "b.go", "c.go"}})
		if err != nil || !ok || len(present) != 1 || present[0] != "b.go" {
			t.Fatalf("got ok=%v present=%v err=%v", ok, present, err)
		}
	})

	t.Run("api error propagates", func(t *testing.T) {
		boom := errors.New("boom")
		fc := &fakeContents{errs: map[string]error{"a.go": boom}}
		_, _, err := Applicable(ctx, fc, Item{Files: []string{"a.go", "b.go"}})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}

func TestJudge(t *testing.T) {
	ctx := context.Background()

	applicable := Item{Kind: KindPR, Title: "fix: thing", Files: []string{"a.go"}, Additions: 2}
	fc := &fakeContents{exists: map[string]bool{"a.go": true}}
	j, err := Judge(ctx, fc, applicable)
	if err != nil {
		t.Fatal(err)
	}
	if j.Class != ClassBugfix || j.Difficulty != DifficultyEasy || !j.Applicable {
		t.Fatalf("judgement = %+v", j)
	}
	if len(j.Present) != 1 || j.Present[0] != "a.go" || j.Reason != "" {
		t.Fatalf("present/reason = %v %q", j.Present, j.Reason)
	}

	missing := Item{Kind: KindPR, Title: "feat: thing", Files: []string{"gone.go"}}
	j2, err := Judge(ctx, &fakeContents{exists: map[string]bool{}}, missing)
	if err != nil {
		t.Fatal(err)
	}
	if j2.Applicable || j2.Reason == "" || j2.Class != ClassFeature {
		t.Fatalf("judgement = %+v", j2)
	}

	boom := errors.New("boom")
	if _, err := Judge(ctx, &fakeContents{errs: map[string]error{"a.go": boom}}, applicable); !errors.Is(err, boom) {
		t.Fatalf("Judge err = %v, want boom", err)
	}
}

func TestGitHubForkContentsExists(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/fork-org/fork-repo/contents/present.go", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"type":"file","name":"present.go","path":"present.go"}`)
	})
	mux.HandleFunc("/repos/fork-org/fork-repo/contents/missing.go", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, `{"message":"Not Found"}`)
	})
	mux.HandleFunc("/repos/fork-org/fork-repo/contents/boom.go", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, `{"message":"boom"}`)
	})

	fc := NewGitHubForkContents(newTestClient(t, mux), "fork-org", "fork-repo")
	ctx := context.Background()

	if ok, err := fc.Exists(ctx, "present.go"); err != nil || !ok {
		t.Fatalf("present: ok=%v err=%v", ok, err)
	}
	if ok, err := fc.Exists(ctx, "missing.go"); err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
	if _, err := fc.Exists(ctx, "boom.go"); err == nil {
		t.Fatal("boom: expected error")
	}
}
