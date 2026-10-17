package hivestate

import (
	"testing"
	"time"
)

// hourSpec describes the synthetic hourly bucket for an hour; ago is how many
// days back from now the hour is (fractional).
type hourSpec struct {
	issues, prs, hold int
	slaFrac           float64 // fraction of the 4 samples violating
	idle, running     int     // agent-samples across the bucket
	kicks             int
	tokens            int64
	mode              string
}

// synth builds hourly buckets from now-days to now, calling f with days-ago.
func synth(now time.Time, days int, f func(ago float64) hourSpec) []Bucket {
	var out []Bucket
	for h := days*24 - 1; h >= 0; h-- {
		start := now.Add(-time.Duration(h)*time.Hour - 30*time.Minute)
		s := f(float64(h) / 24)
		b := Bucket{
			Start: start, Last: start, Samples: 4,
			QueueIssues: []int{s.issues, s.issues, s.issues, s.issues},
			QueuePRs:    []int{s.prs, s.prs, s.prs, s.prs},
			QueueHold:   []int{s.hold, s.hold, s.hold, s.hold},
			Kicks:       s.kicks, AgentSamples: s.idle + s.running, IdleAgentSamples: s.idle,
			Tokens:             s.tokens,
			SLAViolatedSamples: int(s.slaFrac * 4),
		}
		if s.mode != "" {
			b.ModeSamples = map[string]int{s.mode: 4}
		}
		out = append(out, b)
	}
	return out
}

func kinds(obs []Observation) []string {
	var k []string
	for _, o := range obs {
		k = append(k, o.Kind)
	}
	return k
}

func TestObserve(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	base := hourSpec{issues: 10, prs: 5, hold: 2, idle: 1, running: 3, kicks: 1, tokens: 1000}
	tests := []struct {
		name string
		days int
		f    func(ago float64) hourSpec
		want string // expected kind or "" for none
	}{
		{"queue rising", 14, func(a float64) hourSpec {
			s := base
			if a < 7 {
				s.issues, s.prs = 20, 10
			}
			return s
		}, KindQueueGrowth},
		{"queue flat", 14, func(float64) hourSpec { return base }, ""},
		{"queue recovering", 14, func(a float64) hourSpec {
			s := base
			if a >= 7 {
				s.issues, s.prs = 30, 15
			}
			return s
		}, ""},
		{"queue prior window missing", 7, func(a float64) hourSpec {
			s := base
			s.issues = 100
			return s
		}, ""},
		{"hold growing", 7, func(a float64) hourSpec {
			s := base
			s.hold = 10 - int(a)
			return s
		}, KindHoldGrowth},
		{"hold recovering", 7, func(a float64) hourSpec {
			s := base
			s.hold = 3 + int(a)
			return s
		}, ""},
		{"hold gap in days", 7, func(a float64) hourSpec {
			s := base
			s.hold = 10 - int(a)
			return s
		}, KindHoldGrowth},
		{"sla above threshold", 7, func(float64) hourSpec {
			s := base
			s.slaFrac = 0.5
			return s
		}, KindSLA},
		{"sla rising", 14, func(a float64) hourSpec {
			s := base
			if a < 7 {
				s.slaFrac = 0.25
			}
			return s
		}, KindSLA},
		{"sla flat low", 14, func(float64) hourSpec {
			s := base
			s.slaFrac = 0.25
			return s
		}, ""},
		{"sla recovering", 14, func(a float64) hourSpec {
			s := base
			if a >= 7 {
				s.slaFrac = 0.5
			}
			return s
		}, ""},
		{"agents idle", 7, func(float64) hourSpec {
			s := base
			s.idle, s.running = 19, 1
			return s
		}, KindAgentIdle},
		{"agents thrashing", 7, func(float64) hourSpec {
			s := base
			s.idle, s.running, s.kicks = 19, 1, 30
			return s
		}, KindThrashing},
		{"agents busy", 7, func(float64) hourSpec {
			s := base
			s.kicks = 30
			return s
		}, ""},
		{"agents recovering", 7, func(a float64) hourSpec {
			s := base
			if a > 5 {
				s.idle, s.running = 19, 1
			}
			return s
		}, ""},
		{"tokens rising flat queue", 14, func(a float64) hourSpec {
			s := base
			if a < 7 {
				s.tokens = 2000
			}
			return s
		}, KindTokens},
		{"tokens rising queue rising", 14, func(a float64) hourSpec {
			s := base
			if a < 7 {
				s.tokens, s.issues = 2000, 14
			}
			return s
		}, ""},
		{"tokens flat", 14, func(float64) hourSpec { return base }, ""},
		{"tokens recovering", 14, func(a float64) hourSpec {
			s := base
			if a >= 7 {
				s.tokens = 3000
			}
			return s
		}, ""},
		{"mode stuck surge", 7, func(float64) hourSpec {
			s := base
			s.mode = "surge"
			return s
		}, KindModeStuck},
		{"mode stuck idle", 14, func(float64) hourSpec {
			s := base
			s.mode = "idle"
			return s
		}, KindModeStuck},
		{"mode alternating", 7, func(a float64) hourSpec {
			s := base
			s.mode = "surge"
			if int(a*24)%2 == 0 {
				s.mode = "quiet"
			}
			return s
		}, ""},
		{"mode stuck busy", 7, func(float64) hourSpec {
			s := base
			s.mode = "busy"
			return s
		}, ""},
		{"mode recovered", 7, func(a float64) hourSpec {
			s := base
			s.mode = "surge"
			if a < 1 {
				s.mode = "quiet"
			}
			return s
		}, ""},
		{"mode absent", 7, func(float64) hourSpec { return base }, ""},
		{"insufficient coverage", 3, func(float64) hourSpec {
			s := base
			s.slaFrac, s.idle, s.running, s.kicks = 1, 19, 1, 30
			return s
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buckets := synth(now, tt.days, tt.f)
			if tt.name == "hold gap in days" {
				var kept []Bucket
				for _, b := range buckets {
					if age := now.Sub(b.Start); age < 24*time.Hour || age >= 48*time.Hour {
						kept = append(kept, b)
					}
				}
				buckets, tt.want = kept, ""
			}
			got := kinds(Observe(buckets, now, "h1"))
			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("observations = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("observations = %v, want [%s]", got, tt.want)
			}
		})
	}
}

func TestObserveShape(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	if got := Observe(nil, now, "h1"); got != nil {
		t.Fatalf("empty series: %v", got)
	}
	buckets := synth(now, 14, func(a float64) hourSpec {
		s := hourSpec{issues: 10, prs: 5, idle: 1, running: 3, tokens: 1000}
		if a < 7 {
			s.issues, s.prs = 20, 10
		}
		return s
	})
	// Unsorted input is accepted and not mutated.
	first := buckets[0]
	buckets[0], buckets[len(buckets)-1] = buckets[len(buckets)-1], buckets[0]
	got := Observe(buckets, now, "h1")
	if len(got) != 1 {
		t.Fatalf("observations = %v", kinds(got))
	}
	o := got[0]
	if o.Fingerprint != "h1:"+KindQueueGrowth || o.Title == "" || o.Evidence == "" || o.SuspectedCause == "" || o.NextStep == "" {
		t.Fatalf("observation = %+v", o)
	}
	if len(o.Series) != 14 || o.Series[0] != 15 || o.Series[13] != 30 {
		t.Fatalf("series = %v", o.Series)
	}
	if !buckets[len(buckets)-1].Start.Equal(first.Start) {
		t.Fatal("input mutated")
	}
}

func TestDailyBucketsSkipsOutOfRange(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	got := dailyBuckets([]Bucket{
		FromSnapshot(now.Add(time.Hour), Snapshot{}),
		FromSnapshot(now.Add(-5*24*time.Hour), Snapshot{}),
		FromSnapshot(now.Add(-time.Hour), Snapshot{}),
	}, now, 2)
	if got[0].Samples != 0 || got[1].Samples != 1 {
		t.Fatalf("daily = %+v", got)
	}
}

func TestObserveModeStuckShape(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	got := Observe(synth(now, 14, func(float64) hourSpec {
		return hourSpec{issues: 10, prs: 5, idle: 1, running: 3, tokens: 1000, mode: "surge"}
	}), now, "h1")
	if len(got) != 1 {
		t.Fatalf("observations = %v", kinds(got))
	}
	o := got[0]
	if o.Fingerprint != "h1:"+KindModeStuck || o.Title != "Governor mode has been stuck in surge for 7 days" || o.Evidence == "" {
		t.Fatalf("observation = %+v", o)
	}
	if len(o.Series) != 14 || o.Series[0] != 1 || o.Series[13] != 1 {
		t.Fatalf("series = %v", o.Series)
	}
}
