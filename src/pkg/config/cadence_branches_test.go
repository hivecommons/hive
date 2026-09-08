package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// rawTimesCadence builds a Cadence directly in its object encoding, bypassing
// cadenceFromObject validation, so tests can exercise the defensive branches
// that guard against malformed persisted values.
func rawCadence(t *testing.T, obj cadenceObject) Cadence {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal raw cadence: %v", err)
	}
	return Cadence(cadenceObjectPrefix + string(b))
}

func TestCadenceIsPausedFalseForScheduledModes(t *testing.T) {
	var times Cadence
	if err := yaml.Unmarshal([]byte(`{times: ["09:00"], tz: UTC}`), &times); err != nil {
		t.Fatal(err)
	}
	if times.IsPaused() {
		t.Fatal("times cadence must never report paused")
	}
	var cron Cadence
	if err := yaml.Unmarshal([]byte(`{cron: "30 9 * * *", tz: UTC}`), &cron); err != nil {
		t.Fatal(err)
	}
	if cron.IsPaused() {
		t.Fatal("cron cadence must never report paused")
	}
}

func TestCadenceIntervalStringAndMarshalBranches(t *testing.T) {
	c := NewIntervalCadence(" 15m ")
	if got := c.String(); got != "15m" {
		t.Fatalf("interval String() = %q, want 15m", got)
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"15m"` {
		t.Fatalf("interval MarshalJSON = %s, want \"15m\"", data)
	}
	if got := c.HumanSummary(); got != "15m" {
		t.Fatalf("interval HumanSummary = %q", got)
	}
	if got := c.ShortLabel(time.Now()); got != "15m" {
		t.Fatalf("interval ShortLabel = %q", got)
	}
	if specs := c.cronSpecs(); specs != nil {
		t.Fatalf("interval cronSpecs = %v, want nil", specs)
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "15m" {
		t.Fatalf("interval MarshalYAML = %q, want scalar 15m", out)
	}
}

func TestCadenceMarshalYAMLObjectForm(t *testing.T) {
	var c Cadence
	if err := yaml.Unmarshal([]byte(`{times: ["09:00"], tz: UTC}`), &c); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "times:") || !strings.Contains(string(out), "tz: UTC") {
		t.Fatalf("times cadence YAML = %q, want object form", out)
	}
}

func TestCadenceUnmarshalRejectsWrongShapes(t *testing.T) {
	var c Cadence
	if err := yaml.Unmarshal([]byte(`{times: notalist, tz: UTC}`), &c); err == nil {
		t.Fatal("YAML with scalar times should fail to decode")
	}
	if err := json.Unmarshal([]byte(`[1, 2]`), &c); err == nil {
		t.Fatal("JSON array is neither string nor cadence object; want error")
	}
}

func TestCadenceValidateTimezoneAndDayErrors(t *testing.T) {
	var c Cadence
	err := json.Unmarshal([]byte(`{"times":["09:00"]}`), &c)
	if err == nil || !strings.Contains(err.Error(), "timezone (tz) is required") {
		t.Fatalf("missing tz error = %v", err)
	}
	err = json.Unmarshal([]byte(`{"times":["09:00"],"days":["noday"],"tz":"UTC"}`), &c)
	if err == nil || !strings.Contains(err.Error(), "invalid cadence day") {
		t.Fatalf("bad day error = %v", err)
	}
}

func TestCadenceNextAfterFailureBranches(t *testing.T) {
	// Invalid schedule fails Validate and yields no next occurrence.
	bad := rawCadence(t, cadenceObject{Times: []string{"09:00"}})
	if _, ok := bad.NextAfter(time.Now()); ok {
		t.Fatal("invalid cadence must have no next occurrence")
	}
	// Unparseable and non-positive intervals have no next occurrence.
	if _, ok := NewIntervalCadence("nonsense").NextAfter(time.Now()); ok {
		t.Fatal("unparseable interval must have no next occurrence")
	}
	if _, ok := NewIntervalCadence("-5m").NextAfter(time.Now()); ok {
		t.Fatal("negative interval must have no next occurrence")
	}
	if _, ok := NewIntervalCadence("pause").NextAfter(time.Now()); ok {
		t.Fatal("paused interval must have no next occurrence")
	}
}

func TestCadenceDueOccurrenceIntervalWithLastKick(t *testing.T) {
	c := NewIntervalCadence("15m")
	now := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

	due, ok := c.DueOccurrence(now.Add(-20*time.Minute), now, 0)
	if !ok || !due.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("overdue interval: due=%v ok=%v", due, ok)
	}
	if _, ok := c.DueOccurrence(now.Add(-5*time.Minute), now, 0); ok {
		t.Fatal("interval kicked 5m ago must not be due yet")
	}
}

func TestCadenceDueOccurrenceTimesDefaultWindowAndNothingDue(t *testing.T) {
	var c Cadence
	if err := yaml.Unmarshal([]byte(`{times: ["09:00"], tz: UTC}`), &c); err != nil {
		t.Fatal(err)
	}
	// 12:00 UTC is far outside the default 10m catch-up window around 09:00;
	// passing 0 exercises the default-window branch.
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if _, ok := c.DueOccurrence(time.Time{}, now, 0); ok {
		t.Fatal("no occurrence within catch-up window; must not be due")
	}
	// Just after 09:00 with no prior kick, the 09:00 occurrence is due.
	now = time.Date(2026, 8, 7, 9, 1, 0, 0, time.UTC)
	due, ok := c.DueOccurrence(time.Time{}, now, 0)
	if !ok || due.Hour() != 9 || due.Minute() != 0 {
		t.Fatalf("expected 09:00 occurrence due, got %v ok=%v", due, ok)
	}
}

func TestCadenceHumanSummaryKeepsUnparseableTimeVerbatim(t *testing.T) {
	c := rawCadence(t, cadenceObject{Times: []string{"bad", "09:00"}, TZ: "UTC"})
	got := c.HumanSummary()
	if !strings.Contains(got, "bad") || !strings.Contains(got, "9:00 AM") {
		t.Fatalf("summary = %q, want verbatim bad time alongside formatted one", got)
	}
	if !strings.Contains(got, "every day") {
		t.Fatalf("summary = %q, want every-day default", got)
	}
}

func TestCadenceShortLabelFallsBackToConfiguredTZ(t *testing.T) {
	// An invalid schedule makes TimezoneAbbrev return "", so ShortLabel must
	// fall back to the configured tz string.
	times := rawCadence(t, cadenceObject{Times: []string{"bad"}, TZ: "UTC"})
	if tz := times.TimezoneAbbrev(time.Now()); tz != "" {
		t.Fatalf("invalid cadence TimezoneAbbrev = %q, want empty", tz)
	}
	if got := times.ShortLabel(time.Now()); !strings.Contains(got, "UTC") {
		t.Fatalf("times label = %q, want configured tz fallback", got)
	}
	cron := rawCadence(t, cadenceObject{Cron: "not a cron", TZ: "UTC"})
	if got := cron.ShortLabel(time.Now()); got != "🕘 cron UTC" {
		t.Fatalf("cron label = %q, want tz fallback", got)
	}
}

func TestCadenceCronSpecsSkipUnparseableTimes(t *testing.T) {
	c := rawCadence(t, cadenceObject{Times: []string{"bad", "09:30"}, TZ: "UTC"})
	specs := c.cronSpecs()
	if len(specs) != 1 || !strings.Contains(specs[0], "30 9 * * *") {
		t.Fatalf("cronSpecs = %v, want single spec for the valid time", specs)
	}
}

func TestDaysSummaryDefaultsAndJoinHumanSmallCases(t *testing.T) {
	if got := daysSummary(nil); got != "every day" {
		t.Fatalf("daysSummary(nil) = %q", got)
	}
	if got := daysSummary([]string{"bogus"}); got != "every day" {
		t.Fatalf("daysSummary(invalid) = %q", got)
	}
	if got := daysSummary([]string{"mon", "tue", "wed", "thu", "fri"}); got != "Mon–Fri" {
		t.Fatalf("daysSummary(weekdays) = %q", got)
	}
	if got := joinHuman([]string{"only"}); got != "only" {
		t.Fatalf("joinHuman single = %q", got)
	}
	if got := joinHuman([]string{"a", "b"}); got != "a and b" {
		t.Fatalf("joinHuman pair = %q", got)
	}
}
