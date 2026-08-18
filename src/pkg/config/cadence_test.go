package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestCadenceYAMLStringAndObjectForms(t *testing.T) {
	var mode ModeConfig
	if err := yaml.Unmarshal([]byte(`
threshold: 0
scanner: 15m
reviewer:
  times: ["09:00", "17:00"]
  days: ["mon", "tue", "wed", "thu", "fri"]
  tz: America/New_York
quiet:
  cron: "30 9 * * 1-5"
  tz: America/New_York
`), &mode); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mode.Cadences["scanner"] != "15m" {
		t.Fatalf("string cadence = %q", mode.Cadences["scanner"])
	}
	if got := mode.Cadences["reviewer"].HumanSummary(); !strings.Contains(got, "Mon–Fri") || !strings.Contains(got, "America/New_York") {
		t.Fatalf("summary = %q", got)
	}
	data, err := json.Marshal(mode.Cadences["quiet"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"cron"`) {
		t.Fatalf("cron cadence did not marshal as object: %s", data)
	}
}

func TestCadenceRejectsInvalidForms(t *testing.T) {
	cases := []string{
		`{interval: 5m, times: ["09:00"], tz: UTC}`,
		`{times: ["25:00"], tz: UTC}`,
		`{times: ["09:00"], tz: Mars/Olympus}`,
		`{cron: "nope", tz: UTC}`,
		`{times: [], tz: UTC}`,
	}
	for _, tc := range cases {
		var c Cadence
		if err := yaml.Unmarshal([]byte(tc), &c); err == nil {
			t.Fatalf("expected %s to be rejected", tc)
		}
	}
}

func TestCadenceNextOccurrenceTimeZonesAndDays(t *testing.T) {
	var c Cadence
	if err := yaml.Unmarshal([]byte(`{times: ["09:00", "17:00"], days: ["mon","tue","wed","thu","fri"], tz: Pacific/Auckland}`), &c); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Pacific/Auckland")
	next, ok := c.NextAfter(time.Date(2026, 8, 7, 16, 0, 0, 0, loc)) // Friday
	if !ok {
		t.Fatal("no next")
	}
	if next.Weekday() != time.Friday || next.Hour() != 17 {
		t.Fatalf("next = %v, want same Friday 17:00 NZ", next)
	}
	next, _ = c.NextAfter(time.Date(2026, 8, 7, 18, 0, 0, 0, loc))
	if next.Weekday() != time.Monday || next.Hour() != 9 {
		t.Fatalf("next = %v, want Monday 09:00 NZ", next)
	}
}

func TestCadenceDSTSpringForwardSkipsNonexistentTime(t *testing.T) {
	var c Cadence
	if err := yaml.Unmarshal([]byte(`{times: ["02:30"], tz: America/New_York}`), &c); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/New_York")
	next, ok := c.NextAfter(time.Date(2026, 3, 8, 0, 0, 0, 0, loc))
	if !ok {
		t.Fatal("no next")
	}
	if next.Day() == 8 {
		t.Fatalf("spring-forward nonexistent 02:30 should be skipped, got %v", next)
	}
}

func TestCadenceDSTFallBackFiresBothRepeatedTimes(t *testing.T) {
	var c Cadence
	if err := yaml.Unmarshal([]byte(`{times: ["01:30"], tz: America/New_York}`), &c); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/New_York")
	first, ok := c.NextAfter(time.Date(2026, 11, 1, 0, 0, 0, 0, loc))
	if !ok {
		t.Fatal("no first occurrence")
	}
	second, ok := c.NextAfter(first.Add(time.Minute))
	if !ok {
		t.Fatal("no second occurrence")
	}
	if first.Hour() != 1 || second.Hour() != 1 || !second.After(first) || first.Format("MST") == second.Format("MST") {
		t.Fatalf("fall-back occurrences = %v then %v, want both 01:30 in different zones", first, second)
	}
}

func TestCadenceDueOccurrenceAdvancesPastAlreadyKickedOccurrence(t *testing.T) {
	var c Cadence
	if err := yaml.Unmarshal([]byte(`{times: ["09:00", "09:05"], tz: UTC}`), &c); err != nil {
		t.Fatal(err)
	}
	last := time.Date(2026, 8, 7, 9, 0, 2, 0, time.UTC)
	now := time.Date(2026, 8, 7, 9, 5, 4, 0, time.UTC)
	occ, ok := c.DueOccurrence(last, now, CadenceCatchUpWindow)
	if !ok || occ.Hour() != 9 || occ.Minute() != 5 {
		t.Fatalf("occurrence = %v ok=%v, want 09:05 due", occ, ok)
	}
}

func TestCadenceHelpersAndJSONRoundTrip(t *testing.T) {
	if !NewIntervalCadence("pause").IsPaused() {
		t.Fatal("pause interval should be paused")
	}
	if NewIntervalCadence("15m").IsPaused() {
		t.Fatal("15m interval should not be paused")
	}
	intervalNext, ok := NewIntervalCadence("15m").NextAfter(time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC))
	if !ok || intervalNext.Minute() != 15 {
		t.Fatalf("interval next = %v ok=%v", intervalNext, ok)
	}
	occ, ok := NewIntervalCadence("15m").DueOccurrence(time.Time{}, time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC), 0)
	if !ok || occ.IsZero() {
		t.Fatalf("interval first due = %v ok=%v", occ, ok)
	}

	var c Cadence
	if err := json.Unmarshal([]byte(`{"times":["09:00","17:00"],"days":["mon","wed"],"tz":"America/New_York"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Mode() != CadenceModeTimes || !strings.Contains(c.String(), "times:") {
		t.Fatalf("unexpected cadence mode/string: %s %q", c.Mode(), c.String())
	}
	label := c.ShortLabel(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(label, "🕘") || !strings.Contains(label, "09:00,17:00") {
		t.Fatalf("short label = %q", label)
	}
	ny, _ := time.LoadLocation("America/New_York")
	if tz := c.TimezoneAbbrev(time.Date(2026, 1, 1, 0, 0, 0, 0, ny)); tz != "EST" {
		t.Fatalf("timezone abbrev = %q", tz)
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"times"`) {
		t.Fatalf("json = %s", data)
	}

	var interval Cadence
	if err := json.Unmarshal([]byte(`"5m"`), &interval); err != nil {
		t.Fatal(err)
	}
	if interval != "5m" {
		t.Fatalf("interval json = %q", interval)
	}

	var cron Cadence
	if err := json.Unmarshal([]byte(`{"cron":"30 9 * * 1-5","tz":"UTC"}`), &cron); err != nil {
		t.Fatal(err)
	}
	if got := cron.HumanSummary(); !strings.Contains(got, "Cron") || !strings.Contains(got, "UTC") {
		t.Fatalf("cron summary = %q", got)
	}
	if label := cron.ShortLabel(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); !strings.Contains(label, "cron") {
		t.Fatalf("cron label = %q", label)
	}
}

func TestCadenceInvalidJSONAndUtilityBranches(t *testing.T) {
	var c Cadence
	if err := json.Unmarshal([]byte(`{"interval":"5m","cron":"30 9 * * *","tz":"UTC"}`), &c); err == nil {
		t.Fatal("expected JSON mutual exclusion error")
	}
	if _, _, err := parseTimeOfDay("xx"); err == nil {
		t.Fatal("expected malformed time error")
	}
	if _, _, err := parseTimeOfDay("24:00"); err == nil {
		t.Fatal("expected bad hour error")
	}
	if _, _, err := parseTimeOfDay("23:99"); err == nil {
		t.Fatal("expected bad minute error")
	}
	if _, err := normalizedDays([]string{"sunday", "7", "tues", "thur", "bad"}); err == nil {
		t.Fatal("expected bad day error")
	}
	if got := daysSummary([]string{"sat", "sun"}); got != "Sun, Sat" {
		t.Fatalf("days summary = %q", got)
	}
	if got := joinHuman([]string{}); got != "" {
		t.Fatalf("empty join = %q", got)
	}
	if got := joinHuman([]string{"a", "b", "c"}); got != "a, b, and c" {
		t.Fatalf("join = %q", got)
	}
}
