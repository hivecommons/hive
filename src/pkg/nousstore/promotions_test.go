package nousstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var promoteNow = time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)

func TestPromotionPaths(t *testing.T) {
	l := New("/data/nous")
	if got := l.PromotionsDir(); got != "/data/nous/promotions" {
		t.Errorf("PromotionsDir = %q", got)
	}
	if got := l.PromotionHistoryPath(); got != "/data/nous/promotions/history.jsonl" {
		t.Errorf("PromotionHistoryPath = %q", got)
	}
}

func TestValidPromoteTarget(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{PromoteTargetIssue, true}, {PromoteTargetSpektacularRun, true},
		{"", false}, {"Issue", false}, {"pr", false}, {"sim2real", false},
	} {
		if got := ValidPromoteTarget(tc.in); got != tc.want {
			t.Errorf("ValidPromoteTarget(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	targets := PromoteTargets()
	if len(targets) != 2 || targets[0] != PromoteTargetIssue || targets[1] != PromoteTargetSpektacularRun {
		t.Fatalf("PromoteTargets = %v", targets)
	}
	for _, target := range targets {
		if !ValidPromoteTarget(target) {
			t.Errorf("listed target %q is not valid", target)
		}
	}
}

func TestPromotionRoundTrip(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))

	if all, err := l.Promotions(); err != nil || all != nil {
		t.Fatalf("empty history = %v, %v", all, err)
	}
	if _, ok, err := l.FindPromotion("governor", PromoteTargetIssue); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}

	first := Promotion{
		Campaign:       "governor",
		Target:         PromoteTargetIssue,
		Recommendation: "deploy_with_caveats",
		PromotedBy:     "alice",
		Repo:           "myorg/repo1",
		Number:         42,
		URL:            "https://github.com/myorg/repo1/issues/42",
	}
	if err := l.RecordPromotion(first, promoteNow); err != nil {
		t.Fatal(err)
	}
	second := Promotion{
		Campaign:   "governor",
		Target:     PromoteTargetSpektacularRun,
		PromotedBy: "alice",
		PromotedAt: promoteNow.Add(time.Minute).In(time.FixedZone("x", 3600)),
		Repo:       "myorg/repo1",
		Number:     42,
		RunKey:     "myorg/repo1#42",
	}
	if err := l.RecordPromotion(second, promoteNow); err != nil {
		t.Fatal(err)
	}

	all, err := l.Promotions()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("history = %+v, want 2 entries", all)
	}
	if !all[0].PromotedAt.Equal(promoteNow) || all[0].PromotedAt.Location() != time.UTC {
		t.Fatalf("zero PromotedAt not stamped with now in UTC: %v", all[0].PromotedAt)
	}
	if !all[1].PromotedAt.Equal(promoteNow.Add(time.Minute)) {
		t.Fatalf("explicit PromotedAt not kept: %v", all[1].PromotedAt)
	}
	if all[1].RunKey != "myorg/repo1#42" || all[0].URL != first.URL {
		t.Fatalf("history = %+v", all)
	}

	got, ok, err := l.FindPromotion("governor", PromoteTargetIssue)
	if err != nil || !ok || got.Number != 42 || got.Recommendation != "deploy_with_caveats" {
		t.Fatalf("FindPromotion(issue) = %+v ok=%v err=%v", got, ok, err)
	}
	got, ok, err = l.FindPromotion("governor", PromoteTargetSpektacularRun)
	if err != nil || !ok || got.RunKey != "myorg/repo1#42" {
		t.Fatalf("FindPromotion(run) = %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, err := l.FindPromotion("other", PromoteTargetIssue); err != nil || ok {
		t.Fatalf("FindPromotion(other campaign) ok=%v err=%v", ok, err)
	}
}

// The most recent promotion of a target wins when the record holds several.
func TestFindPromotionReturnsLatest(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	for _, n := range []int{1, 2} {
		if err := l.RecordPromotion(Promotion{Campaign: "c", Target: PromoteTargetIssue, PromotedBy: "o", Number: n}, promoteNow); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := l.FindPromotion("c", PromoteTargetIssue)
	if err != nil || !ok || got.Number != 2 {
		t.Fatalf("FindPromotion = %+v ok=%v err=%v, want number 2", got, ok, err)
	}
}

func TestRecordPromotionValidation(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	for name, p := range map[string]Promotion{
		"bad target":  {Campaign: "c", Target: "pr", PromotedBy: "o"},
		"no campaign": {Target: PromoteTargetIssue, PromotedBy: "o"},
		"no promoter": {Campaign: "c", Target: PromoteTargetIssue},
	} {
		err := l.RecordPromotion(p, promoteNow)
		if !errors.Is(err, ErrPromotionMalformed) {
			t.Errorf("%s: err = %v, want ErrPromotionMalformed", name, err)
		}
	}
	if _, err := os.Stat(l.PromotionHistoryPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected promotions must not touch the history: stat err = %v", err)
	}
}

// One bad line cannot hide the rest of the record.
func TestPromotionsSkipsUnparseableLines(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	if err := l.RecordPromotion(Promotion{Campaign: "c", Target: PromoteTargetIssue, PromotedBy: "o", Number: 7}, promoteNow); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(l.PromotionHistoryPath(), os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	all, err := l.Promotions()
	if err != nil || len(all) != 1 || all[0].Number != 7 {
		t.Fatalf("Promotions = %+v, %v", all, err)
	}
}

func TestPromotionsReadErrors(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	// A directory where the history file should be is a read error, not
	// "no promotions".
	if err := os.MkdirAll(l.PromotionHistoryPath(), dirMode); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Promotions(); err == nil || !strings.Contains(err.Error(), "read nous promotion history") {
		t.Fatalf("Promotions err = %v", err)
	}
	if _, _, err := l.FindPromotion("c", PromoteTargetIssue); err == nil {
		t.Fatal("FindPromotion must surface the read error")
	}
	if err := l.RecordPromotion(Promotion{Campaign: "c", Target: PromoteTargetIssue, PromotedBy: "o"}, promoteNow); err == nil || !strings.Contains(err.Error(), "open nous promotion history") {
		t.Fatalf("RecordPromotion onto a directory err = %v", err)
	}
}

func TestRecordPromotionDirCreateError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nous")
	// A regular file where the promotions dir must go blocks MkdirAll.
	if err := os.MkdirAll(root, dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, PromotionsSubdir), []byte("x"), fileMode); err != nil {
		t.Fatal(err)
	}
	err := New(root).RecordPromotion(Promotion{Campaign: "c", Target: PromoteTargetIssue, PromotedBy: "o"}, promoteNow)
	if err == nil || !strings.Contains(err.Error(), "create nous promotions dir") {
		t.Fatalf("err = %v", err)
	}
}
