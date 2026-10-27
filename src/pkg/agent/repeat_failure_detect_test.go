package agent

import (
	"testing"
	"time"
)

func TestRepeatFailureThresholdClearAndRaise(t *testing.T) {
	d := NewRepeatFailureDetector(0, 0, nil)
	now := time.Unix(10000, 0)
	failure := RepeatFailureResult{Failed: true}
	for i := 0; i < 4; i++ {
		if d.Record("a", "false", failure, now) != nil {
			t.Fatal("early note")
		}
	}
	if note := d.Record("a", "false", failure, now); note == nil || note.Count != 5 {
		t.Fatalf("threshold note: %+v", note)
	}
	if d.Record("a", "false", failure, now) != nil {
		t.Fatal("duplicate raise")
	}
	if got := d.Notes("a", now); len(got) != 1 || got[0].Count != 6 {
		t.Fatalf("notes: %+v", got)
	}
	d.Record("a", "false", RepeatFailureResult{}, now)
	if len(d.Notes("a", now)) != 0 {
		t.Fatal("success did not clear")
	}
	for i := 0; i < 5; i++ {
		note := d.Record("a", "false", failure, now)
		if (note != nil) != (i == 4) {
			t.Fatal("incorrect re-raise")
		}
	}
	if len(d.Notes("a", now.Add(10*time.Minute))) != 0 {
		t.Fatal("note did not expire at boundary")
	}
}

func TestRepeatFailureWindowAndResultIsolation(t *testing.T) {
	now := time.Unix(10000, 0)
	one, two := 1, 2
	d := NewRepeatFailureDetector(5, 10*time.Minute, []string{})
	for i := 0; i < 5; i++ {
		// Five failures spread over eleven minutes cannot cross the threshold.
		if d.Record("a", "slow", RepeatFailureResult{Failed: true}, now.Add(time.Duration(i)*165*time.Second)) != nil {
			t.Fatal("outside window")
		}
	}
	d.Forget("a")
	for i := 0; i < 4; i++ {
		d.Record("a", "cmd", RepeatFailureResult{ExitCode: &one}, now)
		d.Record("a", "cmd", RepeatFailureResult{ExitCode: &two}, now)
	}
	if len(d.Notes("a", now)) != 0 {
		t.Fatal("exit statuses merged")
	}
	if d.Record("b", "cmd", RepeatFailureResult{ExitCode: &one}, now) != nil {
		t.Fatal("agents merged")
	}
	note := d.Record("a", "cmd", RepeatFailureResult{ExitCode: &one}, now)
	if note == nil || note.Count != 5 {
		t.Fatalf("note: %+v", note)
	}
	// Mutating an input or returned pointer must not corrupt stored state.
	one = 9
	*note.Result.ExitCode = 8
	notes := d.Notes("a", now)
	if *notes[0].Result.ExitCode != 1 {
		t.Fatal("result alias")
	}
	*notes[0].Result.ExitCode = 7
	if *d.Notes("a", now)[0].Result.ExitCode != 1 {
		t.Fatal("snapshot alias")
	}
	zero := 0
	d.Record("a", "cmd", RepeatFailureResult{ExitCode: &zero, Failed: true}, now)
	if len(d.Notes("a", now)) != 0 {
		t.Fatal("numeric success did not clear")
	}
}

func TestRepeatFailureDistinctCommandsAndExclusions(t *testing.T) {
	now := time.Unix(10000, 0)
	d := NewRepeatFailureDetector(0, 0, nil)
	failure := RepeatFailureResult{Failed: true}
	for i := 0; i < 10; i++ {
		for _, cmd := range []string{"gh pr checks", "gh pr checks 123 --watch", "gh run watch 123"} {
			if d.Record("a", cmd, failure, now) != nil {
				t.Fatal("excluded command raised")
			}
		}
	}
	for i := 0; i < 5; i++ {
		d.Record("a", "gh pr view 123", failure, now)
		d.Record("a", "gh pr view 456", failure, now.Add(time.Second))
	}
	notes := d.Notes("a", now.Add(time.Second))
	if len(notes) != 2 || notes[0].Command != "gh pr view 456" {
		t.Fatalf("distinct commands/newest first: %+v", notes)
	}
	d.Forget("a")
	if len(d.Notes("a", now)) != 0 {
		t.Fatal("forget retained observations")
	}
	custom := NewRepeatFailureDetector(1, time.Minute, []string{"wait "})
	if custom.Record("a", "wait ready", failure, now) != nil {
		t.Fatal("custom exclusion ignored")
	}
	if custom.Record("a", "gh pr checks", failure, now) == nil {
		t.Fatal("explicit exclusion list ignored")
	}
}

func TestRepeatFailureWindowCount(t *testing.T) {
	now := time.Unix(10000, 0)
	d := NewRepeatFailureDetector(2, time.Minute, []string{})
	failure := RepeatFailureResult{Failed: true}
	d.Record("a", "false", failure, now)
	d.Record("a", "false", failure, now.Add(30*time.Second))
	notes := d.Notes("a", now.Add(time.Minute))
	if len(notes) != 1 || notes[0].Count != 1 {
		t.Fatalf("window count: %+v", notes)
	}
	if d.Record("a", "false", failure, now.Add(time.Minute)) != nil {
		t.Fatal("existing note raised twice")
	}
	if len(d.Notes("a", now.Add(2*time.Minute))) != 0 {
		t.Fatal("expiry")
	}
	if len(d.agents) != 0 {
		t.Fatal("expired histories retained")
	}
}
