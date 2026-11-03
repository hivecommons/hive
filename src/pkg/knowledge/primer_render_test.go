package knowledge

import (
	"strconv"
	"strings"
	"testing"
)

// #12124: the primer render is byte-capped the way the code map is. Facts are
// dropped whole at an entry boundary, in priority order, and the output says
// how many were left out.

func renderFacts(n int, bodyLen int) []Fact {
	facts := make([]Fact, 0, n)
	for i := 0; i < n; i++ {
		facts = append(facts, Fact{
			Slug:  "f" + string(rune('a'+i)),
			Title: "Fact " + string(rune('A'+i)),
			Type:  FactGotcha,
			Body:  strings.Repeat("x", bodyLen),
		})
	}
	return facts
}

func TestPrimerRender_UnderCapIsComplete(t *testing.T) {
	pk := &PrimedKnowledge{Facts: renderFacts(5, 100), Hints: []string{"ctx7: /org/lib"}}
	out, omitted := pk.Render()
	if omitted.Any() {
		t.Fatalf("nothing should be omitted under the cap: %+v", omitted)
	}
	if strings.Contains(out, PrimerTruncatedMarker) {
		t.Errorf("marker rendered without truncation:\n%s", out)
	}
	for _, f := range pk.Facts {
		if !strings.Contains(out, "**"+f.Title+"**") {
			t.Errorf("fact %q missing from render", f.Title)
		}
	}
	if !strings.Contains(out, "## Available Documentation") || !strings.Contains(out, "- ctx7: /org/lib") {
		t.Errorf("hints missing from render:\n%s", out)
	}
	if len(out) > DefaultPrimerMaxBytes {
		t.Errorf("render %d bytes exceeds default cap %d", len(out), DefaultPrimerMaxBytes)
	}
	// The pre-cap renderer's shape is preserved so existing kicks look the same.
	if !strings.HasPrefix(out, "# Relevant Knowledge\n\n## gotcha\n\n- **Fact A**\n  ") {
		t.Errorf("render shape changed:\n%s", out)
	}
}

func TestPrimerRender_CapHitDropsWholeFactsInPriorityOrder(t *testing.T) {
	// 20 facts × ~300 B each ≈ 6 KiB, well past a 2 KiB cap.
	pk := &PrimedKnowledge{Facts: renderFacts(20, 280), MaxBytes: 2048}
	out, omitted := pk.Render()
	if !omitted.Any() || omitted.Facts == 0 {
		t.Fatalf("expected facts to be omitted: %+v", omitted)
	}
	if len(out) > 2048 {
		t.Errorf("render %d bytes exceeds cap 2048", len(out))
	}
	// Whole-entry drop: every kept body is intact, no body is cut short.
	kept := strings.Count(out, "\n  "+strings.Repeat("x", 280)+"\n")
	if kept+omitted.Facts != len(pk.Facts) {
		t.Errorf("kept %d + omitted %d != %d facts", kept, omitted.Facts, len(pk.Facts))
	}
	if kept == 0 {
		t.Fatal("cap dropped every fact; at least the first should fit")
	}
	// Priority order preserved: the kept facts are the first N.
	for i := 0; i < kept; i++ {
		if !strings.Contains(out, "**"+pk.Facts[i].Title+"**") {
			t.Errorf("fact %d (%q) should have been kept ahead of lower-ranked facts", i, pk.Facts[i].Title)
		}
	}
	for i := kept; i < len(pk.Facts); i++ {
		if strings.Contains(out, "**"+pk.Facts[i].Title+"**") {
			t.Errorf("fact %d (%q) rendered past the cap", i, pk.Facts[i].Title)
		}
	}
	// Marker is the last line and names the counts.
	want := "_… truncated: " + strconv.Itoa(omitted.Facts) + " facts / " + strconv.Itoa(omitted.Bytes) + " bytes omitted (size cap)._\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("render should end with %q, got tail %q", want, out[len(out)-120:])
	}
	if omitted.Bytes <= 0 {
		t.Error("omitted bytes should be counted")
	}
}

func TestPrimerRender_OversizedFactSkippedSmallerLaterFactKept(t *testing.T) {
	pk := &PrimedKnowledge{
		Facts: []Fact{
			{Slug: "big", Title: "Huge", Type: FactPattern, Body: strings.Repeat("y", 3000)},
			{Slug: "small", Title: "Small", Type: FactPattern, Body: "fits"},
		},
		MaxBytes: 1024,
	}
	out, omitted := pk.Render()
	if omitted.Facts != 1 {
		t.Fatalf("expected exactly the oversized fact omitted, got %+v", omitted)
	}
	if strings.Contains(out, "**Huge**") || strings.Contains(out, "yyyy") {
		t.Errorf("oversized fact must be dropped whole, not clipped:\n%s", out)
	}
	if !strings.Contains(out, "**Small**\n  fits\n") {
		t.Errorf("smaller later fact should still be kept:\n%s", out)
	}
	if !strings.Contains(out, PrimerTruncatedMarker) {
		t.Errorf("marker missing:\n%s", out)
	}
}

func TestPrimerRender_SectionHeadingOnlyWhenAFactIsKept(t *testing.T) {
	pk := &PrimedKnowledge{
		Facts: []Fact{
			{Slug: "a", Title: "A", Type: FactRegression, Body: strings.Repeat("a", 400)},
			{Slug: "b", Title: "B", Type: FactDecision, Body: strings.Repeat("b", 3000)},
		},
		MaxBytes: 1024,
	}
	out, omitted := pk.Render()
	if omitted.Facts != 1 {
		t.Fatalf("omitted = %+v", omitted)
	}
	if !strings.Contains(out, "## regression\n") {
		t.Errorf("kept section heading missing:\n%s", out)
	}
	if strings.Contains(out, "## decision") {
		t.Errorf("heading for a section whose only fact was dropped must not render:\n%s", out)
	}
}

func TestPrimerRender_HintsDroppedWholeAndCounted(t *testing.T) {
	pk := &PrimedKnowledge{
		Facts:    renderFacts(1, 100),
		Hints:    []string{"short hint", strings.Repeat("h", 2000)},
		MaxBytes: 1024,
	}
	out, omitted := pk.Render()
	if omitted.Hints != 1 || omitted.Facts != 0 {
		t.Fatalf("expected one hint omitted: %+v", omitted)
	}
	if !strings.Contains(out, "- short hint\n") || strings.Contains(out, "hhhh") {
		t.Errorf("hint handling wrong:\n%s", out)
	}
	if !strings.Contains(out, "1 hints / ") {
		t.Errorf("marker should name the hint count:\n%s", out)
	}
}

func TestPrimerRender_ZeroMaxBytesUsesDefault_FloorApplied(t *testing.T) {
	if got := primerMaxBytes(0); got != DefaultPrimerMaxBytes {
		t.Errorf("zero -> %d, want default %d", got, DefaultPrimerMaxBytes)
	}
	if got := primerMaxBytes(10); got != primerMinBytes {
		t.Errorf("tiny cap -> %d, want floor %d", got, primerMinBytes)
	}
	if got := primerMaxBytes(65536); got != 65536 {
		t.Errorf("configured cap not honoured: %d", got)
	}
}

func TestNewPrimer_DefaultsMaxBytesAndPropagatesToResult(t *testing.T) {
	p := NewPrimer(nil, PrimerConfig{}, testLogger())
	if p.MaxBytes() != DefaultPrimerMaxBytes {
		t.Errorf("MaxBytes = %d, want %d", p.MaxBytes(), DefaultPrimerMaxBytes)
	}
	p2 := NewPrimer(nil, PrimerConfig{MaxBytes: 9000}, testLogger())
	if p2.MaxBytes() != 9000 {
		t.Errorf("configured MaxBytes = %d", p2.MaxBytes())
	}
}
