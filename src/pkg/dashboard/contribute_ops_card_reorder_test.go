package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func TestOpsCardReorderStaticWiring(t *testing.T) {
	body := renderContributePage(t)

	wantIDs := []string{"clankers", "pipeline", "mine", "wall", "models", "work", "runs", "decisions", "queue", "opp", "triage"}
	seen := map[string]int{}
	for _, match := range regexp.MustCompile(`<div[^>\n]+data-ops-card="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		seen[match[1]]++
	}
	for _, id := range wantIDs {
		if seen[id] != 1 {
			t.Errorf("data-ops-card %q appears %d times, want exactly once", id, seen[id])
		}
	}
	if len(seen) != len(wantIDs) {
		t.Fatalf("unexpected data-ops-card ids: got %v, want %v", seen, wantIDs)
	}

	if got := strings.Count(body, `class="ops-grip"`); got != len(wantIDs) {
		t.Fatalf("grip count = %d, want %d", got, len(wantIDs))
	}
	for _, want := range []string{
		`class="ops-grip"`,
		`touch-action:none`,
		`aria-live="polite"`,
		`id="ops-layout-reset"`,
		`hive.ops.layout`,
		`left:['clankers','pipeline']`,
		`main:['mine','wall','models','work','runs','decisions','queue','opp']`,
		`wide:['triage']`,
		`function opsLayoutNormalize`,
		`localStorage.setItem(OPS_LAYOUT_KEY`,
		`localStorage.removeItem(OPS_LAYOUT_KEY`,
		`setPointerCapture`,
		`pointerdown`,
		`pointermove`,
		`pointerup`,
		`pointercancel`,
		`ops-card-ghost`,
		`ops-drop-indicator`,
		`ArrowUp`,
		`ArrowDown`,
		`ArrowLeft`,
		`ArrowRight`,
		`Escape`,
		`initOpsCardLayout();}catch`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing Operations card reorder wiring %q", want)
		}
	}
}
