package evidence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func backfilled() *Bundle {
	b := &Bundle{
		SchemaVersion: SchemaVersion,
		ID:            BundleID("o/r", 7, "h"),
		Repo:          "o/r",
		Number:        7,
		Author:        Author{Login: "alice", Kind: AuthorHuman},
		BaseSHA:       "b",
		HeadSHA:       "h",
		Merge:         &MergeEvent{Actor: "bob", Method: "unrecorded", At: t0, SHA: "m"},
		Backfilled:    true,
		Backfill: &Backfill{
			Source:         "github-api",
			At:             t0.Add(time.Hour),
			State:          BackfillStateMerged,
			AuthorKindRule: "default_human",
			Flags:          []string{BackfillFlagHiveOnHive},
		},
		CreatedAt: t0,
		UpdatedAt: t0,
	}
	return b
}

func TestBackfilledBundleValidatesAndMatchesSchema(t *testing.T) {
	root := loadSchema(t)
	b := backfilled()
	if err := Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if errs := validateSchema(root, root, toGeneric(t, b), "$"); len(errs) > 0 {
		t.Fatalf("schema:\n%s", strings.Join(errs, "\n"))
	}
	raw, _ := json.Marshal(b)
	for _, absent := range []string{`"verdicts"`, `"shadow"`, `"sentinel"`, `"closed_at"`} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("backfilled bundle must not carry %s: %s", absent, raw)
		}
	}
	if !strings.Contains(string(raw), `"backfilled":true`) {
		t.Errorf("missing backfilled marker: %s", raw)
	}

	closed := backfilled()
	closed.Merge = nil
	closed.Backfill.State = BackfillStateClosed
	closed.Backfill.ClosedAt = t0.Add(time.Minute)
	if err := closed.Validate(); err != nil {
		t.Fatalf("closed: %v", err)
	}
	if errs := validateSchema(root, root, toGeneric(t, closed), "$"); len(errs) > 0 {
		t.Fatalf("closed schema:\n%s", strings.Join(errs, "\n"))
	}
}

func TestBackfillValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Bundle)
		want   string
	}{
		{"flag without marker", func(b *Bundle) { b.Backfilled = false }, "backfilled is false"},
		{"missing source", func(b *Bundle) { b.Backfill.Source = "" }, "backfill.source"},
		{"missing at", func(b *Bundle) { b.Backfill.At = time.Time{} }, "backfill.at"},
		{"bad state", func(b *Bundle) { b.Backfill.State = "draft" }, "backfill.state"},
		{"unknown flag", func(b *Bundle) { b.Backfill.Flags = []string{"tiny"} }, "unknown flag"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := backfilled()
			tc.mutate(b)
			err := b.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
	// A live bundle never sets either field and stays valid.
	if err := sample().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaRejectsInvalidBackfill(t *testing.T) {
	root := loadSchema(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"backfilled type", func(m map[string]any) { m["backfilled"] = "yes" }},
		{"state enum", func(m map[string]any) { m["backfill"].(map[string]any)["state"] = "draft" }},
		{"flag enum", func(m map[string]any) { m["backfill"].(map[string]any)["flags"] = []any{"tiny"} }},
		{"unknown field", func(m map[string]any) { m["backfill"].(map[string]any)["verdict"] = "approve" }},
		{"missing source", func(m map[string]any) { delete(m["backfill"].(map[string]any), "source") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := backfilled()
			if err := Seal(b, nil); err != nil {
				t.Fatal(err)
			}
			m := toGeneric(t, b).(map[string]any)
			tc.mutate(m)
			if errs := validateSchema(root, root, m, "$"); len(errs) == 0 {
				t.Fatal("schema accepted an invalid backfilled bundle")
			}
		})
	}
}

func TestChangeRefs(t *testing.T) {
	text := "Reverts hivecommons/hive#40 and reverts #41. Fixes #42, fixes other/repo#43, follow-up to PR #44, regression from #40, fixes #7"
	reverts, followups := ChangeRefs("hivecommons/hive", 7, text)
	if got, want := reverts, []int{40, 41}; !equalInts(got, want) {
		t.Errorf("reverts = %v, want %v", got, want)
	}
	if got, want := followups, []int{42, 44}; !equalInts(got, want) {
		t.Errorf("followups = %v, want %v", got, want)
	}
	if r, f := ChangeRefs("o/r", 1, ""); r != nil || f != nil {
		t.Errorf("empty text linked %v %v", r, f)
	}
}

func TestWithinLinkWindow(t *testing.T) {
	if !WithinLinkWindow(t0, t0.Add(LinkWindow)) {
		t.Error("edge of window should link")
	}
	if WithinLinkWindow(t0, t0.Add(LinkWindow+time.Second)) {
		t.Error("beyond window linked")
	}
	if WithinLinkWindow(t0.Add(time.Second), t0) {
		t.Error("later merge linked as earlier")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
