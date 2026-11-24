package stagelabels

import (
	"fmt"
	"strconv"
	"strings"
)

// Model-owned stage label slugs (hivecommons/hive#19611).
//
// A workflow model state maps onto a built-in stage by number, and until
// #19611 the item always wore the built-in label for that number: the
// TechDocs "Accepted" state wore `stage/020-claimed` though nobody had
// claimed it. A numbered model state may now declare its own label in the
// same number band (`stage/020-accepted`). The number is what the stage
// store, the tracker, anchors and replies key on, so a model slug is read
// as the built-in stage at its number (StageOfLabel), and a label set
// wearing the built-in spelling at that number swaps to the model's on the
// writer's next pass (Plan removes every unwanted `stage/*` label).

// LabelAs renders s's label, spelled as override when override is a valid
// stage label in s's number band (ValidateModelLabel), else the built-in
// label. An empty or mismatched override never changes the stage: it
// yields s.Label(), so a stage clamped to another kind falls back to the
// built-in spelling of the stage actually written.
func (s Stage) LabelAs(override string) string {
	override = strings.ToLower(strings.TrimSpace(override))
	if override == "" || s == None || ValidateModelLabel(s.Number(), override) != nil {
		return s.Label()
	}
	return override
}

// ValidateModelLabel reports why label cannot be the stage label of a
// model state numbered n: it must be `stage/NNN-<slug>` with NNN the
// zero-padded n, n must be a number in Table, and the slug must be
// lowercase letters, digits and single hyphens. A retired spelling
// (Renames) is refused: the reconciler migrates items off it.
func ValidateModelLabel(n int, label string) error {
	if _, ok := stageAtNumber(n); !ok {
		return fmt.Errorf("stage number %d is not in the stage table", n)
	}
	if _, retired := Renames[label]; retired {
		return fmt.Errorf("label %q is a retired stage label spelling", label)
	}
	want := Prefix + numberString(n) + "-"
	slug, ok := strings.CutPrefix(label, want)
	if !ok {
		return fmt.Errorf("label %q must start with %q", label, want)
	}
	if slug == "" || slug[0] == '-' || slug[len(slug)-1] == '-' || strings.Contains(slug, "--") {
		return fmt.Errorf("label %q needs a slug of lowercase letters, digits and single hyphens after %q", label, want)
	}
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("label %q needs a slug of lowercase letters, digits and single hyphens after %q", label, want)
		}
	}
	return nil
}

// StageOfLabel returns the stage a single label carries: the built-in
// spelling, a retired spelling in Renames, or any other `stage/NNN-<slug>`
// whose NNN is a Table number (a model-owned slug, #19611). A label at a
// number the table does not have, or a health marker, is not a stage.
// Matching is case-insensitive.
func StageOfLabel(label string) (Stage, bool) {
	l := strings.ToLower(strings.TrimSpace(label))
	if s, ok := Renames[l]; ok {
		return s, true
	}
	if !IsStageLabel(l) {
		return None, false
	}
	n, err := strconv.Atoi(l[len(Prefix) : len(Prefix)+NumberWidth])
	if err != nil {
		return None, false
	}
	return stageAtNumber(n)
}

// stageAtNumber is the Table stage with number n.
func stageAtNumber(n int) (Stage, bool) {
	if n <= 0 {
		return None, false
	}
	for _, d := range Table {
		if d.Number == n {
			return d.Stage, true
		}
	}
	return None, false
}
