package hubactivity

import (
	"slices"
	"testing"
	"time"
)

func TestNormalizeEvent_BoundsAndValidates(t *testing.T) {
	loc := time.FixedZone("x", 3600)
	in := Event{
		Seq:     7,
		Type:    " STAGE\t",
		Repo:    " o/r ",
		Number:  42,
		Kind:    "PR",
		Title:   "hello\u2028world",
		From:    "Needs Review!",
		Stage:   "Actionable",
		At:      t0.In(loc),
		Private: true,
	}
	got, ok := NormalizeEvent(in)
	if !ok {
		t.Fatal("expected event to be accepted")
	}
	if got.Type != EventStage || got.Repo != "o/r" || got.Kind != "pr" {
		t.Errorf("tokens not normalized: %+v", got)
	}
	if got.Title == in.Title || got.Title == "" {
		t.Errorf("title not cleaned: %q", got.Title)
	}
	if got.From != slug(in.From) || got.Stage != slug(in.Stage) || got.From == "" || got.Stage == "" {
		t.Errorf("from/stage not slugged: %q %q", got.From, got.Stage)
	}
	if got.At.Location() != time.UTC || !got.At.Equal(t0) {
		t.Errorf("At not UTC-normalized: %v", got.At)
	}
	if !got.Private || got.Seq != 7 || got.Number != 42 {
		t.Errorf("fields not carried: %+v", got)
	}
}

func TestNormalizeEvent_RejectsMalformed(t *testing.T) {
	valid := Event{Seq: 1, Type: EventOpened, Repo: "o/r", Number: 1}
	if _, ok := NormalizeEvent(valid); !ok {
		t.Fatal("baseline event must be accepted")
	}
	cases := map[string]func(e *Event){
		"unknown type": func(e *Event) { e.Type = "closed" },
		"empty type":   func(e *Event) { e.Type = "" },
		"zero seq":     func(e *Event) { e.Seq = 0 },
		"bad repo":     func(e *Event) { e.Repo = "not-a-repo" },
		"empty repo":   func(e *Event) { e.Repo = "" },
		"zero number":  func(e *Event) { e.Number = 0 },
		"neg number":   func(e *Event) { e.Number = -3 },
		"huge number":  func(e *Event) { e.Number = maxCount + 1 },
	}
	for name, mutate := range cases {
		e := valid
		mutate(&e)
		got, ok := NormalizeEvent(e)
		if ok {
			t.Errorf("%s: expected rejection, got %+v", name, got)
		}
		if got != (Event{}) {
			t.Errorf("%s: rejected event must be zero, got %+v", name, got)
		}
	}
}

func TestNormalizeEvent_UnknownKindDropped(t *testing.T) {
	got, ok := NormalizeEvent(Event{Seq: 1, Type: EventMerged, Repo: "o/r", Number: 1, Kind: "discussion"})
	if !ok {
		t.Fatal("expected acceptance")
	}
	if got.Kind != "" {
		t.Errorf("unknown kind should be blanked, got %q", got.Kind)
	}
}

func TestSanitizeEvents_DropsMalformedDedupesAndOrders(t *testing.T) {
	in := []Event{
		{Seq: 3, Type: EventStage, Repo: "o/r", Number: 3},
		{Seq: 0, Type: EventStage, Repo: "o/r", Number: 9}, // no seq
		{Seq: 1, Type: EventOpened, Repo: "o/r", Number: 1},
		{Seq: 3, Type: EventMerged, Repo: "o/r", Number: 33}, // duplicate seq
		{Seq: 2, Type: "bogus", Repo: "o/r", Number: 2},      // bad type
		{Seq: 2, Type: EventMerged, Repo: "o/r", Number: 2},
	}
	snapshot := slices.Clone(in)

	out, dropped := SanitizeEvents(in)
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 (cap not hit)", dropped)
	}
	var seqs []uint64
	var nums []int
	for _, e := range out {
		seqs = append(seqs, e.Seq)
		nums = append(nums, e.Number)
	}
	if !slices.Equal(seqs, []uint64{1, 2, 3}) {
		t.Errorf("seqs = %v, want [1 2 3]", seqs)
	}
	if !slices.Equal(nums, []int{1, 2, 3}) {
		t.Errorf("first occurrence must win: numbers = %v, want [1 2 3]", nums)
	}
	if !slices.Equal(in, snapshot) {
		t.Error("SanitizeEvents must not modify its input")
	}
}

func TestSanitizeEvents_CapsAtMaxEventsDroppingOldest(t *testing.T) {
	total := MaxEvents + 25
	in := make([]Event, 0, total)
	for i := total; i >= 1; i-- { // reverse order to exercise sorting too
		in = append(in, Event{Seq: uint64(i), Type: EventStage, Repo: "o/r", Number: i})
	}

	out, dropped := SanitizeEvents(in)
	if dropped != 25 {
		t.Errorf("dropped = %d, want 25", dropped)
	}
	if len(out) != MaxEvents {
		t.Fatalf("len = %d, want %d", len(out), MaxEvents)
	}
	if out[0].Seq != 26 || out[len(out)-1].Seq != uint64(total) {
		t.Errorf("kept range = [%d, %d], want [26, %d]", out[0].Seq, out[len(out)-1].Seq, total)
	}
	for i := 1; i < len(out); i++ {
		if out[i-1].Seq >= out[i].Seq {
			t.Fatalf("not strictly ascending at %d: %d >= %d", i, out[i-1].Seq, out[i].Seq)
		}
	}
}

func TestSanitizeEvents_Empty(t *testing.T) {
	out, dropped := SanitizeEvents(nil)
	if len(out) != 0 || dropped != 0 {
		t.Errorf("got %v, %d; want empty, 0", out, dropped)
	}
}

func TestClampCount(t *testing.T) {
	for in, want := range map[int]int{-5: 0, 0: 0, 7: 7, maxCount: maxCount, maxCount + 1: maxCount} {
		if got := ClampCount(in); got != want {
			t.Errorf("ClampCount(%d) = %d, want %d", in, got, want)
		}
	}
}
